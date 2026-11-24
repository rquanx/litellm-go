package main

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

func withDashboardAssets(next http.Handler, root *os.Root) http.Handler {
	assets := root.FS()
	files := http.FileServerFS(assets)
	mux := http.NewServeMux()
	mux.Handle("GET /litellm-asset-prefix/_next/", http.StripPrefix("/litellm-asset-prefix", files))
	mux.Handle("GET /litellm-asset-prefix/assets/", http.StripPrefix("/litellm-asset-prefix", files))
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		location := "/ui/"
		if r.URL.RawQuery != "" {
			location += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, location, http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /ui/", func(w http.ResponseWriter, r *http.Request) {
		relative := strings.TrimPrefix(r.URL.Path, "/ui/")
		if relative == "" {
			http.ServeFileFS(w, r, assets, "index.html")
			return
		}
		name := strings.TrimSuffix(relative, "/")
		if !fs.ValidPath(name) || strings.Contains(name, "\\") {
			http.NotFound(w, r)
			return
		}
		info, err := fs.Stat(assets, name)
		if err == nil && info.IsDir() {
			index := path.Join(name, "index.html")
			if page, err := fs.Stat(assets, index); err == nil && !page.IsDir() {
				http.ServeFileFS(w, r, assets, index)
				return
			}
		}
		if (err != nil || info.IsDir()) && path.Ext(name) == "" {
			http.ServeFileFS(w, r, assets, name+".html")
			return
		}
		http.StripPrefix("/ui", files).ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /get_image", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "logo.jpg")
	})
	mux.HandleFunc("GET /get_favicon", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "favicon.ico")
	})
	mux.Handle("/", next)
	return mux
}
