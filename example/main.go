package main

import (
	"context"
	"log"
	"os"

	"github.com/cuipeiyu/go-watcher"
)

const ignore = `
# ignore some files and directories
.gitignore
.git
node_modules
.DS_Store
`

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile | log.Ltime)
}

func main() {
	rootPath := "./"

	ctx := context.Background()

	w, err := watcher.New(
		rootPath,
		watcher.WithContext(ctx),
		watcher.WithIgnorePattern(ignore),
	)
	if err != nil {
		panic(err)
	}

	w.Start()

	list := w.WatchedList()
	for path := range list {
		log.Println("watching:", path)
	}

r:
	for {
		select {
		case err := <-w.Errors:
			log.Println("   erorr:", err)
		case e := <-w.Events:
			log.Printf("  change: [%-6s] %s => %s", e.Op.String(), e.FromPath, e.ToPath)
		case <-ctx.Done():
			w.Close()
			break r
		}
	}

	log.Println("exit")
	os.Exit(0)
}
