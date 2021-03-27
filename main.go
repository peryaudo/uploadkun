package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"syscall"

	"github.com/hanwen/go-fuse/fs"
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
	Entries []JsonEntry `json:"entries"`
}

func addEntry(parent *fs.Inode, entry *JsonEntry) {
	var child *fs.Inode
	switch entry.Kind {
	case File:
		child = parent.NewPersistentInode(context.Background(), &fs.Inode{}, fs.StableAttr{})
	case Directory:
		child = parent.NewPersistentInode(context.Background(), &fs.Inode{}, fs.StableAttr{Mode: syscall.S_IFDIR})
		for _, subEntry := range entry.Entries {
			addEntry(child, &subEntry)
		}
	}
	parent.AddChild(entry.Name, child, true)
}

func main() {
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
		addEntry(root, &rootEntry)
		response := struct {
			Success bool
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
