package webdav

import (
	"net/url"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// dsmSample is shaped like a DSM answer: mixed "D:" and "d:" prefixes between
// responses, a trailing slash and an empty getcontentlength on the collection,
// percent-encoded non-ASCII, and a second propstat carrying the properties the
// server could not produce.
const dsmSample = `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:">
  <D:response>
    <D:href>/media/</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:collection/></D:resourcetype>
        <D:getlastmodified>Mon, 02 Jan 2006 15:04:05 GMT</D:getlastmodified>
        <D:getcontentlength></D:getcontentlength>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <d:response xmlns:d="DAV:">
    <d:href>/media/Filme%20A-Z/Gr%C3%BC%C3%9Fe.mkv</d:href>
    <d:propstat>
      <d:prop>
        <d:resourcetype/>
        <d:getcontentlength>1234567</d:getcontentlength>
        <d:getlastmodified>Tue, 03 Jan 2006 15:04:05 GMT</d:getlastmodified>
      </d:prop>
      <d:status>HTTP/1.1 200 OK</d:status>
    </d:propstat>
    <d:propstat>
      <d:prop><d:getetag/></d:prop>
      <d:status>HTTP/1.1 404 Not Found</d:status>
    </d:propstat>
  </d:response>
</D:multistatus>`

func TestDecodeMultistatus(t *testing.T) {
	entries, nonNFC, err := decodeMultistatus(strings.NewReader(dsmSample), "/media")
	if err != nil {
		t.Fatalf("decodeMultistatus: %v", err)
	}
	if nonNFC != 0 {
		t.Errorf("nonNFC = %d, want 0", nonNFC)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	root := entries[0]
	if root.Path != "" || !root.IsDir || root.Size != 0 {
		t.Errorf("root = %+v, want the collection itself with an empty path", root)
	}

	file := entries[1]
	if file.Path != "Filme A-Z/Grüße.mkv" {
		t.Errorf("file.Path = %q, want %q", file.Path, "Filme A-Z/Grüße.mkv")
	}
	if file.Name != "Grüße.mkv" {
		t.Errorf("file.Name = %q, want %q", file.Name, "Grüße.mkv")
	}
	if file.Size != 1234567 {
		t.Errorf("file.Size = %d, want 1234567", file.Size)
	}
	if file.IsDir {
		t.Errorf("file.IsDir = true, want false")
	}
	if got := file.MTime.Format("2006-01-02T15:04:05Z"); got != "2006-01-03T15:04:05Z" {
		t.Errorf("file.MTime = %s, want 2006-01-03T15:04:05Z", got)
	}
}

// TestDecodeMultistatusNFD covers the trap of DESIGN.md §2.3: a name that reached
// the share through macOS is decomposed, and comparing it to an NFC local name
// makes the file look new on every single scan.
func TestDecodeMultistatusNFD(t *testing.T) {
	nfd := norm.NFD.String("Grüße.mkv")
	body := `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:"><D:response><D:href>/media/` + url.PathEscape(nfd) + `</D:href>
<D:propstat><D:prop><D:resourcetype/><D:getcontentlength>1</D:getcontentlength></D:prop>
<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`

	entries, nonNFC, err := decodeMultistatus(strings.NewReader(body), "/media")
	if err != nil {
		t.Fatalf("decodeMultistatus: %v", err)
	}
	if nonNFC != 1 {
		t.Errorf("nonNFC = %d, want 1", nonNFC)
	}
	if len(entries) != 1 || entries[0].Path != "Grüße.mkv" {
		t.Fatalf("entries = %+v, want the NFC form", entries)
	}
}

func TestRelFromHref(t *testing.T) {
	cases := []struct {
		name string
		href string
		root string
		want string
		fail bool
	}{
		{"absolute path", "/media/Foo.mkv", "/media", "Foo.mkv", false},
		{"absolute url", "http://nas:5005/media/Foo.mkv", "/media", "Foo.mkv", false},
		{"collection trailing slash", "/media/Serien/", "/media", "Serien", false},
		{"root itself", "/media/", "/media", "", false},
		{"root without base path", "/", "", "", false},
		{"child without base path", "/Foo.mkv", "", "Foo.mkv", false},
		{"percent encoded", "/media/A%20B/C%2BD.mkv", "/media", "A B/C+D.mkv", false},
		{"outside the root", "/other/Foo.mkv", "/media", "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := relFromHref(c.href, c.root)
			if c.fail {
				if err == nil {
					t.Fatalf("relFromHref(%q, %q) = %q, want an error", c.href, c.root, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("relFromHref(%q, %q): %v", c.href, c.root, err)
			}
			if got != c.want {
				t.Errorf("relFromHref(%q, %q) = %q, want %q", c.href, c.root, got, c.want)
			}
		})
	}
}

func TestCleanRel(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"/":           "",
		"a/b":         "a/b",
		"/a/b/":       "a/b",
		"a//b":        "a/b",
		"a/./b":       "a/b",
		"ClipCornDB/": "ClipCornDB",
	}
	for in, want := range cases {
		if got := cleanRel(in); got != want {
			t.Errorf("cleanRel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusOK(t *testing.T) {
	cases := map[string]bool{
		"HTTP/1.1 200 OK":           true,
		"HTTP/1.1 207 Multi-Status": true,
		"HTTP/1.1 404 Not Found":    false,
		"HTTP/1.1 403 Forbidden":    false,
		"":                          false,
		"nonsense":                  false,
		"HTTP/1.1 xxx Not A Status": false,
	}
	for in, want := range cases {
		if got := statusOK(in); got != want {
			t.Errorf("statusOK(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTotalFromContentRange(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		fail bool
	}{
		{"bytes 0-99/12345", 12345, false},
		{"bytes 100-199/*", -1, false},
		{"", -1, false},
		{"bytes 0-99", 0, true},
		{"bytes 0-99/abc", 0, true},
	}
	for _, c := range cases {
		got, err := totalFromContentRange(c.in)
		if c.fail {
			if err == nil {
				t.Errorf("totalFromContentRange(%q) = %d, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("totalFromContentRange(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("totalFromContentRange(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
