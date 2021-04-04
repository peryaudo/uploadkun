package main

import (
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"syscall"

	"github.com/hanwen/go-fuse/fs"
	"github.com/hanwen/go-fuse/fuse"
)

type JsonEntry struct {
	Name         string `json:"name"`
	Size         uint64 `json:"size"`
	LastModified uint64 `json:"lastModified"`
}

var downloadReq chan *memFileNode

// TODO(tetsui): Use loopback instead of mem file
type memFileNode struct {
	name string
	fs.MemRegularFile
}

func (mfn *memFileNode) Flush(ctx context.Context, f fs.FileHandle) syscall.Errno {
	log.Println("Flush")
	downloadReq <- mfn
	return 0
}

type rootNode struct {
	fs.Inode
}

func (rn *rootNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0755
	out.Uid = uint32(os.Getuid())
	out.Gid = uint32(os.Getgid())
	return 0
}

func (rn *rootNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	log.Println("Create: ", name)

	child := rn.NewPersistentInode(ctx, &memFileNode{name, fs.MemRegularFile{Data: []byte{}, Attr: fuse.Attr{Mode: 0644}}}, fs.StableAttr{})
	rn.AddChild(name, child, true)
	return child, nil, 0, 0
}

var fileReq chan *remoteFileNode

type remoteFileNode struct {
	metadata JsonEntry
	fs.Inode

	cacheReady chan bool
	cacheFile  *os.File
}

func (fn *remoteFileNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0644
	out.Uid = uint32(os.Getuid())
	out.Gid = uint32(os.Getgid())
	out.Atime = fn.metadata.LastModified
	out.Mtime = fn.metadata.LastModified
	out.Ctime = fn.metadata.LastModified
	out.Size = fn.metadata.Size
	const bs = 512
	// out.Blksize = bs
	out.Blocks = (out.Size + bs - 1) / bs
	return 0
}

func (fn *remoteFileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if fn.cacheFile == nil {
		fileReq <- fn
	}
	return nil, 0, 0
}

func (fn *remoteFileNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if fn.cacheFile == nil {
		// TODO: Is there a race?
		fn.cacheReady = make(chan bool)
		<-fn.cacheReady
		fn.cacheReady = nil
	}
	return fuse.ReadResultFd(fn.cacheFile.Fd(), off, len(dest)), 0
}

func (fn *remoteFileNode) Release(ctx context.Context) syscall.Errno {
	if fn.cacheFile != nil {
		if err := fn.cacheFile.Close(); err != nil {
			log.Println("Release: ", err)
			return syscall.EIO
		}
		if err := os.Remove(fn.cacheFile.Name()); err != nil {
			log.Println("Release: ", err)
			return syscall.EIO
		}
	}
	return 0
}

func updateEntries(root *rootNode, entries []JsonEntry) {
	entriesMap := make(map[string]bool)
	for _, entry := range entries {
		entriesMap[entry.Name] = true
	}
	removedMap := make(map[string]*fs.Inode)
	for name, child := range root.Children() {
		if !entriesMap[name] {
			removedMap[name] = child
		}
	}
	for name, child := range removedMap {
		root.NotifyDelete(name, child)
		root.RmChild(name)
	}

	for _, entry := range entries {
		if child := root.GetChild(entry.Name); child != nil {
			if fn, ok := child.Operations().(*remoteFileNode); ok {
				fn.metadata = entry
			}
			// TODO(tetsui): Handle when it's a file that has been just downloaded
			continue
		}
		child := root.NewPersistentInode(
			context.Background(), &remoteFileNode{metadata: entry}, fs.StableAttr{})
		root.AddChild(entry.Name, child, true)
		root.NotifyEntry(entry.Name)
	}
}

func checkSequenceNumber(seq int, w http.ResponseWriter, r *http.Request) bool {
	if expectedSeq, err := strconv.Atoi(r.URL.Query().Get("seq")); err != nil {
		log.Println("malformed sequence number")
		http.Error(w, "malformed sequence number", http.StatusBadRequest)
		return false
	} else if expectedSeq != seq {
		log.Println("sequence number does not match")
		http.Error(w, "sequence number does not match", http.StatusBadRequest)
		return false
	}
	return true
}

func main() {
	if len(os.Args) < 3 {
		log.Fatalln("usage: uploadkun mount_point http_port")
	}
	mntDir := os.Args[1]
	httpPort := os.Args[2]

	fileReq = make(chan *remoteFileNode)
	downloadReq = make(chan *memFileNode)
	root := &rootNode{}

	var downFileNode *memFileNode

	seq := 0

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})
	http.HandleFunc("/notifyEntries", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("seq") == "" {
			seq++
		} else if !checkSequenceNumber(seq, w, r) {
			return
		}
		var entries []JsonEntry
		if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
			log.Println("notifyEntries: ", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		updateEntries(root, entries)
		response := struct {
			Success bool `json:"success"`
			Seq     int  `json:"seq"`
		}{
			Success: true,
			Seq:     seq,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
	http.HandleFunc("/pollFileRequest", func(w http.ResponseWriter, r *http.Request) {
		if !checkSequenceNumber(seq, w, r) {
			return
		}
		select {
		case fn := <-fileReq:
			if !checkSequenceNumber(seq, w, r) {
				return
			}
			response := struct {
				Kind     string `json:"kind"`
				Filename string `json:"filename"`
			}{
				Kind:     "upload",
				Filename: fn.metadata.Name,
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
		case mfn := <-downloadReq:
			if !checkSequenceNumber(seq, w, r) {
				return
			}
			downFileNode = mfn
			response := struct {
				Kind string `json:"kind"`
			}{
				Kind: "download",
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
		}
	})
	http.HandleFunc("/downloadFile", func(w http.ResponseWriter, r *http.Request) {
		if !checkSequenceNumber(seq, w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+downFileNode.name+"\"")
		if _, err := w.Write(downFileNode.Data); err != nil {
			log.Println("downloadFile: failed to write")
			return
		}
	})
	http.HandleFunc("/uploadFile", func(w http.ResponseWriter, r *http.Request) {
		if !checkSequenceNumber(seq, w, r) {
			return
		}
		r.ParseMultipartForm(100 * 1024 * 1024)
		f, _, err := r.FormFile("f")
		if err != nil {
			log.Println("uploadFile: ", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		tmpfile, err := ioutil.TempFile("", "uploadedFile")
		if err != nil {
			log.Println("uploadFile: ", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := io.Copy(tmpfile, f); err != nil {
			log.Println("uploadFile: ", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		filename := r.FormValue("filename")
		node := root.GetChild(filename)
		if node == nil {
			log.Printf("uploadFile: file %q not found\n", filename)
			http.Error(w, "no matching file found", http.StatusInternalServerError)
			return
		}
		fn := node.Operations().(*remoteFileNode)
		fn.cacheFile = tmpfile
		if fn.cacheReady != nil {
			fn.cacheReady <- true
		}

		response := struct {
			Success bool `json:"success"`
		}{
			Success: true,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
	go func() {
		log.Println("Listening on port " + httpPort + "...")
		log.Fatal(http.ListenAndServe("127.0.0.1:"+httpPort, nil))
	}()

	server, err := fs.Mount(mntDir, root, &fs.Options{})
	if err != nil {
		log.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		log.Printf("unmount by mount %s\n", mntDir)
	} else if runtime.GOOS == "linux" {
		log.Printf("unmount by fusermount -u %s\n", mntDir)
	}
	server.Wait()
}
