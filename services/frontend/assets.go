package frontend

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/styles.css
var staticAssets embed.FS

func StaticHandler() http.Handler {
	subFS, err := fs.Sub(staticAssets, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(subFS))
}
