package main

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"syscall"
	"strconv"

	"github.com/hanwen/go-fuse/fs"
	"github.com/hanwen/go-fuse/fuse"
)

type EntryKind int

const (
	File EntryKind = iota
	Directory
)

func (k *EntryKind) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch s {
	case "file":
		*k = File
	case "directory":
		*k = Directory
	}
	return nil
}

type JsonEntry struct {
	Kind         EntryKind   `json:"kind"`
	Name         string      `json:"name"`
	Size         uint64      `json:"size"`
	LastModified uint64      `json:"lastModified"`
	Entries      []JsonEntry `json:"entries"`
}

type dirNode struct {
	metadata *JsonEntry
	fs.Inode
}

func (dn *dirNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0755
	out.Uid = uint32(os.Getuid())
	out.Gid = uint32(os.Getgid())
	return 0
}

func (dn *dirNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	log.Println("Create: ", name)

	child := dn.NewPersistentInode(ctx, &memFileNode{name, fs.MemRegularFile{Data: []byte{}, Attr: fuse.Attr{Mode: 0644}}}, fs.StableAttr{})
	dn.AddChild(name, child, true)
	return child, nil, 0, 0
}

var downloadReq chan *memFileNode

type memFileNode struct {
	name string
	fs.MemRegularFile
}

func (mfn *memFileNode) Flush(ctx context.Context, f fs.FileHandle) syscall.Errno {
	log.Println("Flush: ", mfn.Data)
	downloadReq <- mfn
	return 0
}

var fileReq chan *fileNode

type fileNode struct {
	metadata *JsonEntry
	loc      []string
	fs.Inode

	cacheReady chan bool
	cache      []byte
}

func (fn *fileNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
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

func (fn *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if fn.cache == nil {
		fileReq <- fn
	}
	return nil, 0, 0
}

func (fn *fileNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if fn.cache == nil {
		// TODO: Is there a race?
		fn.cacheReady = make(chan bool)
		<-fn.cacheReady
		fn.cacheReady = nil
	}
	end := int(off) + len(dest)
	if end > len(fn.cache) {
		end = len(fn.cache)
	}
	return fuse.ReadResultData(fn.cache[off:end]), 0
}

func addEntry(parent *fs.Inode, entry *JsonEntry, loc []string) {
	loc = append(loc, entry.Name)
	var child *fs.Inode
	switch entry.Kind {
	case File:
		child = parent.NewPersistentInode(context.Background(), &fileNode{metadata: entry, loc: loc}, fs.StableAttr{})
	case Directory:
		child = parent.NewPersistentInode(context.Background(), &dirNode{metadata: entry}, fs.StableAttr{Mode: syscall.S_IFDIR})
		for _, subEntry := range entry.Entries {
			addEntry(child, &subEntry, loc)
		}
	}
	parent.AddChild(entry.Name, child, true)
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
	fileReq = make(chan *fileNode)
	downloadReq = make(chan *memFileNode)
	root := &fs.Inode{}

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
		var rootEntry JsonEntry
		if err := json.NewDecoder(r.Body).Decode(&rootEntry); err != nil {
			log.Println("notifyEntries: ", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		root.RmAllChildren()
		addEntry(root, &rootEntry, []string{})
		response := struct {
			Success bool `json:"success"`
			Seq int `json:"seq"`
		}{
			Success: true,
			Seq: seq,
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
			// TODO(tetsui): check the sequence number here
			response := struct {
				Kind     string   `json:"kind"`
				Location []string `json:"location"`
			}{
				Kind:     "upload",
				Location: fn.loc,
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
		case mfn := <-downloadReq:
			// TODO(tetsui): ditto
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
		}
		defer f.Close()
		b, err := ioutil.ReadAll(f)
		if err != nil {
			log.Println("uploadFile: ", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		var loc []string
		if err := json.Unmarshal([]byte(r.FormValue("loc")), &loc); err != nil {
			log.Println("uploadFile: ", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		node := root
		for _, l := range loc {
			node = node.GetChild(l)
			if node == nil {
				log.Println("uploadFile: not found...")
				http.Error(w, "no matching file found", http.StatusInternalServerError)
				return
			}
		}
		fn, ok := node.Operations().(*fileNode)
		if !ok {
			log.Println("uploadFile: cannot get fileNode")
			http.Error(w, "cannot get fileNode", http.StatusInternalServerError)
			return
		}
		fn.cache = b
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
		log.Fatal(http.ListenAndServe("0.0.0.0:8080", nil))
	}()

	mntDir := "/tmp/x"

	server, err := fs.Mount(mntDir, root, &fs.Options{})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("unmount by fusermount -u %s\n", mntDir)
	server.Wait()
}
