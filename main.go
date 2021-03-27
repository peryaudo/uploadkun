package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"syscall"
	"os"

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
	Kind    EntryKind   `json:"kind"`
	Name    string      `json:"name"`
	Size	uint64 `json:"size"`
	LastModified uint64 `json:"lastModified"`
	Entries []JsonEntry `json:"entries"`
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

var fileReq chan *fileNode

type fileNode struct {
	metadata *JsonEntry
	loc []string
	fs.Inode
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
	fileReq <- fn
	return nil, 0, 0
}

func (fn *fileNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	return fuse.ReadResultData(dest), 0
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

func main() {
	fileReq = make(chan *fileNode)
	root := &fs.Inode{}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})
	http.HandleFunc("/notifyEntries", func(w http.ResponseWriter, r *http.Request) {
		var rootEntry JsonEntry
		if err := json.NewDecoder(r.Body).Decode(&rootEntry); err != nil {
			log.Println("notifyEntries: ", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		root.RmAllChildren()
		addEntry(root, &rootEntry, []string{})
		response := struct {
			Success bool `json:"success"`
		}{
			Success: true,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
	http.HandleFunc("/pollFileRequest", func(w http.ResponseWriter, r *http.Request) {
		fn := <- fileReq

		response := struct {
			Location []string `json:"location"`
		}{
			Location: fn.loc,
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
