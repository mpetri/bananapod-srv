package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/goji/httpauth"
	"github.com/mpetri/go-poppler"
	"goji.io"
	"goji.io/pat"
	"golang.org/x/net/context"
	"hash/fnv"
	"image"
	"image/jpeg"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	// "github.com/nfnt/resize"
	// "github.com/mpetri/go-cairo"
)

const (
	ImageDPI      float64 = 600.0
	PointsperInch float64 = 72.0
)

type ArchiveCategory struct {
	Name     string `json:"name"`
	Elements int    `json:"elements"`
}

type ArchiveDoc struct {
	Id         uint64    `json:"id"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	CreateDate time.Time `json:"timestamp"`
	FileDate   time.Time `json:"timestamp"`
	Pages      int       `json:"pages"`
	Content    string    `json:"content"`
	FilePath   string    `json:"-"`
}
type ArchiveDocs []*ArchiveDoc

type ArchiveDocThumb struct {
	Id       uint64
	Thumb    image.Image
	Encoding []byte
}

func (slice ArchiveDocs) Len() int {
	return len(slice)
}

func (slice ArchiveDocs) Less(i, j int) bool {
	return slice[i].FileDate.After(slice[j].FileDate)
}

func (slice ArchiveDocs) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

func (a *ArchiveDoc) String() string {
	return fmt.Sprintf("id:%v name:%v size:%v pages:%v date:%v date:%v content: %v\n", a.Id, a.Name, a.Size, a.Pages, a.CreateDate, a.FileDate, a.Content)
}

var (
	archivePath        = flag.String("archive", "", "path to the document archive")
	port               = flag.Int("port", 8000, "Server port")
	docCache           = make(map[uint64]*ArchiveDoc)
	docCacheMutex      = &sync.Mutex{}
	docThumbCache      = make(map[uint64]*ArchiveDocThumb)
	docThumbCacheMutex = &sync.Mutex{}
)

func ProcessDocument(filepath string) (doc *ArchiveDoc, err error) {
	// generate ID from path
	h := fnv.New64a()
	h.Write([]byte(filepath))
	docId := h.Sum64()

	// check cache
	docCacheMutex.Lock()
	prestoredDoc, ok := docCache[docId]
	docCacheMutex.Unlock()
	if ok {
		return prestoredDoc, nil
	}

	// get file stats
	file, err := os.Open(filepath)
	defer file.Close()
	if err != nil {
		return nil, err
	}
	fi, err := file.Stat()
	if err != nil {
		return nil, err
	}
	fileSize := fi.Size()
	modTime := fi.ModTime()

	// try to parse file time
	var year, month, day, hour, minute, second int
	numparsed, err := fmt.Sscanf(path.Base(filepath), "%d_%d_%d_%d_%d_%d", &year, &month, &day, &hour, &minute, &second)
	fileTime := modTime
	if err == nil && numparsed == 6 {
		now := time.Now()
		fileTime = time.Date(year, time.Month(month), day, hour, minute, second, 0, now.Location())
	}

	// get pdf stats
	pdfDoc, err := poppler.Open(filepath)
	if err != nil {
		return nil, err
	}
	numPages := pdfDoc.GetNPages()

	firstPage := pdfDoc.GetPage(0)
	pageText := firstPage.Text()
	encodedText := base64.StdEncoding.EncodeToString([]byte(pageText))

	newDoc := &ArchiveDoc{
		Id:         docId,
		Name:       path.Base(filepath),
		FilePath:   filepath,
		Size:       fileSize,
		CreateDate: modTime,
		FileDate:   fileTime,
		Pages:      numPages,
		Content:    encodedText,
	}

	// add to cache
	docCacheMutex.Lock()
	docCache[docId] = newDoc
	docCacheMutex.Unlock()

	return newDoc, nil
}

func AllDocs(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	pattern := fmt.Sprintf("%v/*/*.pdf", *archivePath)
	archiveFiles, err := filepath.Glob(pattern)
	if err != nil {
		log.Fatal("Error finding archive content: %v", err.Error())
	}
	log.Printf("Found %v documents (%v)", len(archiveFiles), pattern)

	// (2) parse docs
	log.Printf("Parse %v documents", len(archiveFiles))
	archiveDocs := make(ArchiveDocs, 0, 0)
	for _, file := range archiveFiles {
		newDoc, err := ProcessDocument(file)
		if err != nil {
			log.Printf("Error parsing document %v: %v", file, err.Error())
		} else {
			archiveDocs = append(archiveDocs, newDoc)
		}
	}

	log.Printf("Sort %v documents", len(archiveDocs))
	sort.Sort(archiveDocs)

	log.Printf("Output %v documents", len(archiveDocs))
	json.NewEncoder(w).Encode(archiveDocs)
}

func Categories(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	pattern := fmt.Sprintf("%v/*", *archivePath)
	archiveCategories, err := filepath.Glob(pattern)
	if err != nil {
		log.Fatal("Error finding archive content: %v", err.Error())
	}
	log.Printf("Found %v categories (%v)", len(archiveCategories), pattern)
	archiveCats := make([]*ArchiveCategory, 0, 0)
	for _, category := range archiveCategories {
		cat := path.Base(category)
		isHidden := strings.HasPrefix(cat, ".")
		if isHidden == false {
			f, err := os.Open(category)
			if err != nil {
				log.Fatal("Error finding parsing categories: %v", err.Error())
			}
			defer f.Close()
			fi, err := f.Stat()
			if err != nil {
				log.Fatal("Error finding parsing categories: %v", err.Error())
			}
			mode := fi.Mode()
			if mode.IsDir() == true {
				fileinCat, err := filepath.Glob(category + "/*.pdf")
				if err != nil {
					log.Fatal("Error finding parsing categories: %v", err.Error())
				}
				newCat := &ArchiveCategory{
					Name:     cat,
					Elements: len(fileinCat),
				}
				archiveCats = append(archiveCats, newCat)
			}
		}
	}

	json.NewEncoder(w).Encode(archiveCats)
}

func DocContent(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	id := pat.Param(ctx, "id")

	docCacheMutex.Lock()
	prestoredDoc, ok := docCache[docId]
	docCacheMutex.Unlock()
	if !ok {
		log.Printf("Requesting unknown document: %v", docId)
		fmt.Fprintf(w, "Requesting unknown document: %v", docId)
	}
	in, err := os.Open(prestoredDoc.FilePath)
    if err != nil {
        return
    }
    defer in.Close()

	if _, err = io.Copy(w, in); err != nil {
        log.Printf("Error transfering document: %v", prestoredDoc.FilePath)
    }
}

func DocThumbnail(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	docIdStr := pat.Param(ctx, "id")
	docId, err := strconv.ParseUint(docIdStr, 10, 64)
	if err != nil {
		log.Printf("Invalid docid: %v", docIdStr)
		fmt.Fprintf(w, "Invalid docid: %v", docIdStr)
	}

	docThumbCacheMutex.Lock()
	prestoredThumb, ok := docThumbCache[docId]
	docThumbCacheMutex.Unlock()
	if ok {
		w.Write(prestoredThumb.Encoding)
		return
	}

	docCacheMutex.Lock()
	prestoredDoc, ok := docCache[docId]
	docCacheMutex.Unlock()
	if !ok {
		log.Printf("Requesting unknown document: %v", docId)
		fmt.Fprintf(w, "Requesting unknown document: %v", docId)
	}

	log.Printf("Open pdf document: %v", prestoredDoc.FilePath)
	pdfDoc, err := poppler.Open(prestoredDoc.FilePath)
	if err != nil {
		log.Printf("Error opening PDF document for thumbnail creation: %v", err.Error())
	}
	log.Printf("Get first page of pdf document: %v", prestoredDoc.FilePath)
	firstPage := pdfDoc.GetPage(0)
	log.Printf("Render first page of document: %v", prestoredDoc.FilePath)
	pageImage := firstPage.Render(300)

	log.Printf("Encode as PNG document: %v", prestoredDoc.FilePath)
	var b bytes.Buffer
	buf := bufio.NewWriter(&b)
	jpeg.Encode(buf, pageImage, nil)
	log.Printf("Write to client document: %v", prestoredDoc.FilePath)

	newThumb := &ArchiveDocThumb{
		Id:       docId,
		Thumb:    pageImage,
		Encoding: b.Bytes(),
	}
	log.Printf("Store in cache document: %v", prestoredDoc.FilePath)
	docThumbCacheMutex.Lock()
	docThumbCache[docId] = newThumb
	docThumbCacheMutex.Unlock()

	b.WriteTo(w)
}

func main() {
	flag.Parse()
	log.Printf("Archive path = %v", *archivePath)
	log.Printf("Listening port = %v", *port)

	mux := goji.NewMux()
	mux.Use(httpauth.SimpleBasicAuth("dave", "somepassword"))
	mux.HandleFuncC(pat.Get("/alldocs/"), AllDocs)
	mux.HandleFuncC(pat.Get("/categories/"), Categories)
	mux.HandleFuncC(pat.Get("/thumbnail/:id"), DocThumbnail)
	mux.HandleFuncC(pat.Get("/doc/:id"), DocContent)

	log.Printf("Listening on = 0.0.0.0:%v", *port)
	http.ListenAndServe(fmt.Sprintf("0.0.0.0:%v", *port), mux)
}
