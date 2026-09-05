package httpd

import (
	"html"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// entry is one line of a directory listing.
type entry struct {
	// Name carries a trailing slash for a folder, as the Node implementation's
	// cnam does, because that is also the href the browser follows.
	Name    string
	IsFile  bool
	Kind    string
	Size    int64
	ModTime time.Time
}

// readDirectory lists a folder: the folders first in the order the filesystem
// reports them, then the files newest first.
func readDirectory(folder string) ([]entry, error) {
	items, err := os.ReadDir(folder)
	if err != nil {
		return nil, err
	}

	var folders, files []entry
	for _, item := range items {
		info, err := item.Info()
		if err != nil {
			// a name that vanished between the read and the stat is skipped
			// rather than failing the whole listing
			continue
		}
		found := entry{
			Name:    info.Name(),
			IsFile:  info.Mode().IsRegular(),
			Kind:    typeOf(info.Name()).Kind,
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
		if info.IsDir() {
			found.Name += "/"
			folders = append(folders, found)
			continue
		}
		files = append(files, found)
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].ModTime.After(files[j].ModTime) })
	return append(folders, files...), nil
}

// favicon is the one the Node implementation embeds, a small grey square.
const favicon = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAQAAADhVxYwAAAAmklEQVR42u3BMQEAAAABIP6Pzg" +
	"pV3U2nAlcjcPtm/hJrrfZyzwhjz6a1Im5Pjx9yzdjDZssnAAQy9drmt5EMDtntD8XgG1VYpdDzwDAwAAAABJRU5ErkJggg=="

const listingStyle = `<style>
      .table { display: grid; grid-template-columns: 3fr 1fr 1fr 1fr; gap: 0px; }
      .row { display: contents; }
      .row:hover .cell-content { background: #e0e0e0; }
      .cell-header { padding: 5px; text-align: left; border-bottom: 1px solid #000000; }
      .cell-content { padding: 5px; text-align: left; border: 0px; }
      .cell:nth-child(odd) { background: #f9f9f9; }
    </style>`

// listingPage renders the browsable directory page. The markup is the one the
// Node implementation produces, so the pages look unchanged.
func listingPage(virtual string, entries []entry) []byte {
	var page strings.Builder
	page.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n")
	page.WriteString("<meta charset=\"UTF-8\">\n")
	page.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1.0\">\n")
	page.WriteString("<link rel=\"icon\" type=\"image/png\" href=\"" + favicon + "\" />\n")
	page.WriteString("<title>" + html.EscapeString(virtual) + "</title>\n")
	page.WriteString(listingStyle + "\n</head>\n<body>\n")
	page.WriteString(`<div class="table"><div class="row">` +
		`<div class="cell-header">Name</div><div class="cell-header">Created</div>` +
		`<div class="cell-header">Type</div><div class="cell-header">Size</div></div>` + "\n")

	if parent := parentOf(virtual); parent != "" {
		page.WriteString(row(parent, "../", "-", "dir", "-"))
	}
	for _, item := range entries {
		created, kind, size := "", "Directory", "-"
		if item.IsFile {
			created = item.ModTime.Format("2006.01.02 - 15:04:05")
			kind = item.Kind
			size = readableSize(item.Size)
		}
		page.WriteString(row(item.Name, item.Name, created, kind, size))
	}
	page.WriteString("</div></body></html>\n")
	return []byte(page.String())
}

// row is one line of the grid: a link and three plain cells.
func row(href, name, created, kind, size string) string {
	link := (&url.URL{Path: href}).String()
	return `<div class="row"><div class="cell-content"><a href="` + link + `">` +
		html.EscapeString(name) + `</a></div>` +
		`<div class="cell-content">` + html.EscapeString(created) + `</div>` +
		`<div class="cell-content">` + html.EscapeString(kind) + `</div>` +
		`<div class="cell-content">` + html.EscapeString(size) + `</div></div>` + "\n"
}

// parentOf is the link to the folder above, empty at the root.
func parentOf(virtual string) string {
	cleaned := strings.TrimSuffix(virtual, "/")
	if cleaned == "" || cleaned == "/" {
		return ""
	}
	parent := path.Dir(cleaned)
	if !strings.HasSuffix(parent, "/") {
		parent += "/"
	}
	return parent
}

// readerPage is the answer of the dls_directory_reader endpoint, a plain list
// of links rather than the browsable page.
func readerPage(folder string, entries []entry) []byte {
	var page strings.Builder
	page.WriteString("listing directory: " + html.EscapeString(folder) + "\n")
	for _, item := range entries {
		kind := "dir"
		if item.IsFile {
			kind = "file"
		}
		link := (&url.URL{Path: item.Name}).String()
		page.WriteString(`<a href="` + link + `">` + html.EscapeString(item.Name) + `</a>` +
			" - filetype: " + kind + " filesize: " + itoa(item.Size) + "<br/>\n")
	}
	return []byte(page.String())
}
