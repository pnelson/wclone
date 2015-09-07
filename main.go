package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"

	"golang.org/x/net/html"
)

type asset struct {
	filename string
	ready    chan struct{}
}

type ref struct {
	node  *html.Node
	index int
}

var refTagAttrs = map[string]string{
	"img":    "src",
	"link":   "href",
	"script": "src",
}

var errRefNode = errors.New("wclone: node is not ref")

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "wclone: invalid arguments")
		flag.PrintDefaults()
		os.Exit(1)
	}
	err := clone(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// clone does the best it can to download a single web page
// and its assets, rewriting references to relative paths.
func clone(src string) error {
	resp, err := http.Get(src)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	doc, hash, err := parse(resp.Body)
	if err != nil {
		return err
	}
	dir, err := makeSitesDir(hash)
	if err != nil {
		return err
	}
	err = process(doc, src, dir)
	if err != nil {
		return err
	}
	filename := path.Join(dir, "index.html")
	return save(doc, filename)
}

// parse reads body into a *html.Node and returns the sha1 hash
// of the document with it.
func parse(body io.Reader) (*html.Node, string, error) {
	h := sha1.New()
	r := io.TeeReader(body, h)
	doc, err := html.Parse(r)
	if err != nil {
		return nil, "", err
	}
	hash := hex.EncodeToString(h.Sum(nil))
	return doc, hash, nil
}

// makeSitesDir creates public/sites/:hash if it doesn't already exist.
func makeSitesDir(hash string) (string, error) {
	dir := path.Join("public", "sites", hash)
	_, err := os.Stat(dir)
	if err != nil && !os.IsNotExist(err) {
		return dir, err
	}
	err = os.MkdirAll(dir, 0750)
	if err != nil {
		return dir, err
	}
	return dir, nil
}

// process walks the document and downloads and updates
// references concurrently.
func process(doc *html.Node, src, dir string) error {
	var wg sync.WaitGroup
	base, err := url.Parse(src)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	errc := make(chan error)
	assets := make(map[string]*asset)
	for r := range walk(doc) {
		r := r // shadow for goroutines
		wg.Add(1)
		uri := getRefURI(r)
		a, ok := assets[uri]
		if ok {
			// Asset is already downloaded or in progress.
			// Update asset reference when complete.
			go func() {
				defer wg.Done()
				<-a.ready
				setRefURI(r, a.filename)
			}()
		} else {
			a := &asset{ready: make(chan struct{})}
			assets[uri] = a
			go func() {
				defer wg.Done()
				u, err := resolveReference(uri, base)
				if err != nil {
					errc <- err
					return
				}
				a.filename, err = download(u, dir)
				if err != nil {
					errc <- err
					return
				}
				setRefURI(r, a.filename)
				// Tell waiting refs to update asset URI.
				close(a.ready)
			}()
		}
	}
	go func() {
		wg.Wait()
		done <- struct{}{}
	}()
	// Wait for all goroutines to complete or the first error.
	select {
	case err := <-errc:
		return err
	case <-done:
	}
	return nil
}

// walk traverses n and returns a channel of asset references.
func walk(n *html.Node) <-chan *ref {
	rs := make(chan *ref)
	go func() {
		walkNode(n, rs)
		close(rs)
	}()
	return rs
}

// walkNode inspects n for valid asset references and sends
// them to rs before traversing n's children.
func walkNode(n *html.Node, rs chan *ref) {
	r, err := newRef(n)
	if err == nil {
		rs <- r
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkNode(c, rs)
	}
}

// newRef creates a new ref from n if n is a ref node and valid.
func newRef(n *html.Node) (*ref, error) {
	if n.Type != html.ElementNode {
		return nil, errRefNode
	}
	key, ok := refTagAttrs[n.Data]
	if !ok {
		return nil, errRefNode
	}
	i := getAttrIndex(n, key)
	if i == -1 {
		return nil, errRefNode
	}
	r := &ref{node: n, index: i}
	if !isValidRef(r) {
		return nil, errRefNode
	}
	return r, nil
}

// getRefURI returns r's attribute value.
func getRefURI(r *ref) string {
	return r.node.Attr[r.index].Val
}

// setRefURI sets r's attribute value to uri.
func setRefURI(r *ref, uri string) {
	r.node.Attr[r.index].Val = uri
}

// getAttrIndex returns the index of the key attribute within n's attributes.
func getAttrIndex(n *html.Node, key string) int {
	for i, a := range n.Attr {
		if a.Key == key {
			return i
		}
	}
	return -1
}

// isValidRef returns true if r is considered valid.
// A ref is valid if it has the extension of an asset.
func isValidRef(r *ref) bool {
	uri := getRefURI(r)
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	ext := path.Ext(u.Path)
	return isAsset(ext)
}

// isAsset returns true if ext is to be considered an asset.
func isAsset(ext string) bool {
	exts := []string{".css", ".gif", ".jpeg", ".jpg", ".js", ".png"}
	for _, e := range exts {
		if e == ext {
			return true
		}
	}
	return false
}

// resolveReference resolves a URI reference to an absolute URI
// from an absolute base URI.
func resolveReference(uri string, base *url.URL) (*url.URL, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	return base.ResolveReference(u), nil
}

// download saves a copy of u into dir.
// The asset is streamed directly to disk and then
// renamed to the sha1 hash plus the file extension
// as the hash is only known after all bytes have been read.
func download(u *url.URL, dir string) (string, error) {
	uri := u.String()
	ext := path.Ext(u.Path)
	resp, err := http.Get(uri)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	h := sha1.New()
	r := io.TeeReader(resp.Body, h)
	f, err := ioutil.TempFile(dir, ".")
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	if err != nil {
		return "", err
	}
	hash := hex.EncodeToString(h.Sum(nil))
	base := hash + ext
	filename := path.Join(dir, base)
	err = os.Rename(f.Name(), filename)
	if err != nil {
		return "", err
	}
	return base, nil
}

// save persists doc to filename on disk.
func save(doc *html.Node, filename string) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	err = html.Render(f, doc)
	if err != nil {
		return err
	}
	return nil
}
