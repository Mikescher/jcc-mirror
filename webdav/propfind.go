package webdav

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"blackforestbytes.com/jcc-mirror/remote"
	"golang.org/x/text/unicode/norm"
)

// propfindBody asks for the three properties the mirror needs and nothing else.
// allprop makes DSM return a dozen Synology-specific fields per entry, which on a
// walk of a hundred thousand files is pure transfer volume for no information.
const propfindBody = `<?xml version="1.0" encoding="utf-8"?>` + "\n" +
	`<propfind xmlns="DAV:"><prop><getcontentlength/><getlastmodified/><resourcetype/></prop></propfind>`

// multistatus mirrors the 207 response body. The tags carry the "DAV:" namespace
// rather than an element prefix, so DSM answering with "D:" on one request and
// "d:" on the next - which it does - decodes identically either way.
type multistatus struct {
	XMLName   xml.Name     `xml:"DAV: multistatus"`
	Responses []msResponse `xml:"DAV: response"`
}

type msResponse struct {
	Href      string       `xml:"DAV: href"`
	Propstats []msPropstat `xml:"DAV: propstat"`
}

type msPropstat struct {
	Status string `xml:"DAV: status"`
	Prop   msProp `xml:"DAV: prop"`
}

type msProp struct {
	// ContentLength is a string rather than an int64 because collections carry an
	// empty element on some servers, and a strict int64 decode of that fails the
	// whole document instead of one entry.
	ContentLength string     `xml:"DAV: getcontentlength"`
	LastModified  string     `xml:"DAV: getlastmodified"`
	ResourceType  *msResType `xml:"DAV: resourcetype"`
}

type msResType struct {
	Collection *struct{} `xml:"DAV: collection"`
}

// decodeMultistatus turns a 207 body into entries relative to root, the decoded
// path of the base URL. The response describing root itself is included, with an
// empty Path; callers that listed a directory drop it.
//
// nonNFC counts the hrefs that were not already NFC. It is worth knowing rather
// than silently repairing: a source tree that is partly NFD is what makes every
// title with an umlaut look new on every scan (DESIGN.md §2.3).
func decodeMultistatus(r io.Reader, root string) (entries []remote.Entry, nonNFC int, err error) {
	var ms multistatus
	if err := xml.NewDecoder(r).Decode(&ms); err != nil {
		return nil, 0, fmt.Errorf("decode multistatus: %w", err)
	}

	out := make([]remote.Entry, 0, len(ms.Responses))
	for _, resp := range ms.Responses {
		e, normalized, ok, err := decodeResponse(resp, root)
		if err != nil {
			return nil, 0, err
		}
		if normalized {
			nonNFC++
		}
		if !ok {
			continue
		}
		out = append(out, e)
	}
	return out, nonNFC, nil
}

// decodeResponse converts one <response>. ok is false when no propstat succeeded,
// which is how a server reports a property it could not produce.
func decodeResponse(resp msResponse, root string) (entry remote.Entry, normalized, ok bool, err error) {
	rel, normalized, err := relFromHref(resp.Href, root)
	if err != nil {
		return remote.Entry{}, false, false, err
	}

	prop, ok := okProp(resp.Propstats)
	if !ok {
		return remote.Entry{}, normalized, false, nil
	}

	e := remote.Entry{
		Path:  rel,
		Name:  path.Base("/" + rel),
		IsDir: prop.ResourceType != nil && prop.ResourceType.Collection != nil,
	}
	if e.IsDir && rel == "" {
		e.Name = ""
	}

	if s := strings.TrimSpace(prop.ContentLength); s != "" && !e.IsDir {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return remote.Entry{}, normalized, false, fmt.Errorf("getcontentlength %q of %q: %w", s, rel, err)
		}
		e.Size = n
	}

	if s := strings.TrimSpace(prop.LastModified); s != "" {
		// http.ParseTime accepts all three legal HTTP date formats; getlastmodified
		// is specified as RFC 1123 in GMT, so this lands on a whole second.
		t, err := http.ParseTime(s)
		if err != nil {
			return remote.Entry{}, normalized, false, fmt.Errorf("getlastmodified %q of %q: %w", s, rel, err)
		}
		e.MTime = t.UTC()
	}

	return e, normalized, true, nil
}

// okProp returns the properties of the first propstat with a 2xx status.
func okProp(pss []msPropstat) (msProp, bool) {
	for _, ps := range pss {
		if statusOK(ps.Status) {
			return ps.Prop, true
		}
	}
	return msProp{}, false
}

// statusOK parses a propstat status line, e.g. "HTTP/1.1 200 OK".
func statusOK(s string) bool {
	f := strings.Fields(s)
	if len(f) < 2 {
		return false
	}
	code, err := strconv.Atoi(f[1])
	return err == nil && code >= 200 && code < 300
}

// relFromHref turns a multistatus href into a path relative to root. The href may
// be an absolute URL or an absolute path and is always percent-encoded; root is
// the decoded path of the base URL. Root itself yields "". normalized reports
// whether the href had to be converted to NFC.
func relFromHref(href, root string) (rel string, normalized bool, err error) {
	u, err := url.Parse(href)
	if err != nil {
		return "", false, fmt.Errorf("parse href %q: %w", href, err)
	}
	if u.Path == "" {
		return "", false, fmt.Errorf("empty href in multistatus response")
	}

	// Clean drops the trailing slash collections are returned with, so directories
	// and files end up in the same shape.
	p := path.Clean("/" + u.Path)
	r := path.Clean("/" + root)

	if p == r {
		return "", false, nil
	}
	if r != "/" {
		if !strings.HasPrefix(p, r+"/") {
			return "", false, fmt.Errorf("href %q lies outside the remote root %q", u.Path, root)
		}
		p = p[len(r)+1:]
	} else {
		p = strings.TrimPrefix(p, "/")
	}

	return norm.NFC.String(p), !norm.NFC.IsNormalString(p), nil
}

// cleanRel normalizes a caller-supplied relative path into the form relFromHref
// produces, so the two can be compared.
func cleanRel(rel string) string {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return ""
	}
	return norm.NFC.String(strings.TrimPrefix(path.Clean("/"+rel), "/"))
}
