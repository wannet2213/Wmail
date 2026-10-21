package api

import (
	"io/fs"
	"net/http/httptest"
	"regexp"
	"testing"
	"testing/fstest"
)

func TestPagesVersionTheirAssets(t *testing.T) {
	web := fstest.MapFS{
		"index.html":     {Data: []byte(`<link rel="stylesheet" href="assets/app.css"><script src="assets/app.js"></script>`)},
		"assets/app.css": {Data: []byte("css")},
		"assets/app.js":  {Data: []byte("v1")},
	}
	get := func(web fs.FS, path string) string {
		rec := httptest.NewRecorder()
		staticHandler(web).ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Body.String()
	}
	page := get(web, "/")
	if !regexp.MustCompile(`href="assets/app\.css\?v=\w{12}".*src="assets/app\.js\?v=\w{12}"`).MatchString(page) {
		t.Fatalf("asset URLs not versioned: %s", page)
	}
	web["assets/app.js"] = &fstest.MapFile{Data: []byte("v2")}
	if get(web, "/") == page {
		t.Fatal("a changed script must get a new URL, or browsers keep the cached one")
	}
	if got := get(web, "/assets/app.js?v=abc"); got != "v2" {
		t.Fatalf("versioned asset URL served %q", got)
	}
}
