package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "net/http/pprof"

	"github.com/PuerkitoBio/goquery"
	"github.com/bradfitz/http2"
	"github.com/jackc/pgx"
	_ "github.com/jackc/pgx/stdlib"
	"github.com/jmoiron/sqlx"
)

var (
	pt = fmt.Printf
	sp = fmt.Sprintf
)

var schema = `
CREATE TABLE video (
	id int primary key,
	title text
);
`

func main() {
	checkErr := func(msg string, err error) {
		if err != nil {
			log.Fatalf("%s error %v", msg, err)
		}
	}

	db, err := sqlx.Connect("pgx", "postgres://reus@localhost/bilibili")
	checkErr("connect to psql", err)
	err = db.Ping()
	checkErr("ping database", err)
	//db.MustExec(schema)

	client := http.Client{
		Timeout: time.Second * 8,
	}
	get := func(url string) []byte {
		resp, err := client.Get(url)
		checkErr(sp("get %s", url), err)
		defer resp.Body.Close()
		content, err := ioutil.ReadAll(resp.Body)
		checkErr(sp("read %s", url), err)
		return content
	}

	bytesToDoc := func(bs []byte) *goquery.Document {
		doc, err := goquery.NewDocumentFromReader(bytes.NewReader(bs))
		checkErr("get document", err)
		return doc
	}

	collectFollowers := func() []string {

	getMaxPage:
		doc := bytesToDoc(get("http://space.bilibili.com/19415/follow.html?page=1"))
		maxPage := doc.Find("ul.page li").Length()
		if maxPage == 0 {
			goto getMaxPage
		}
		users := []string{}
		collectUser := func(url string) {
		getDoc:
			doc := bytesToDoc(get(url))
			entries := doc.Find("div.fanslist ul li div.t a[card]")
			if entries.Length() == 0 {
				goto getDoc
			}
			entries.Each(func(n int, se *goquery.Selection) {
				href, ok := se.Attr("href")
				if !ok {
					log.Fatal("invalid entry")
				}
				uid := href[strings.LastIndex(href, "/")+1:]
				users = append(users, uid)
			})
		}
		for i := 1; i <= maxPage; i++ {
			collectUser(sp("http://space.bilibili.com/19415/follow.html?page=%d", i))
		}
		return users
	}

	collectVideo := func(uid string, page int) (int, int) {
		url := sp("http://space.bilibili.com/space?uid=%s&page=%d", uid, page)
		doc := bytesToDoc(get(url))
		entries := doc.Find("div.main_list ul li")
		count := 0
		dup := 0
		entries.Each(func(n int, se *goquery.Selection) {
			titleSe := se.Find("a.title")
			title := titleSe.Text()
			href, ok := titleSe.Attr("href")
			if !ok || len(title) == 0 {
				log.Fatal("invalid entry")
			}
			idStr := href[strings.LastIndex(href, "av")+2:]
			idStr = idStr[:len(idStr)-1]
			id, err := strconv.Atoi(idStr)
			imgSe := se.Find("a img")
			image, ok := imgSe.Attr("src")
			if !ok {
				log.Fatal("no image")
			}
			_, err = db.Exec(`INSERT INTO video (id, title, image, added) 
				VALUES ($1, $2, $3, $4)`, id, title, image, time.Now().Unix())
			if err != nil {
				if err.(pgx.PgError).Code == "23505" { // dup
					dup++
				} else {
					log.Fatal(err)
				}
			}
			count++
		})
		return count, dup
	}

	collect := func() {
		t0 := time.Now()
		users := collectFollowers()
		for _, uid := range users {
			page := 1
			totalDup := 0
			for {
				count, dup := collectVideo(uid, page)
				if count == 0 {
					break
				}
				totalDup += dup
				if totalDup > 50 {
					break
				}
				page++
			}
		}
		pt("collected in %v\n", time.Now().Sub(t0))
	}

	go func() {
		for {
			collect()
			time.Sleep(time.Minute * 5)
		}
	}()

	root := "./web"
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	root, err = filepath.Abs(root)
	checkErr("get web root dir", err)
	pt("web root %s\n", root)
	http.Handle("/", http.FileServer(http.Dir(root)))

	http.HandleFunc("/newest.json", func(w http.ResponseWriter, req *http.Request) {
		videos := []Video{}
		err := db.Select(&videos, `SELECT id, title, image FROM video 
			WHERE view < 1
			ORDER BY added DESC, id DESC LIMIT 50`)
		checkErr("select", err)
		bs, err := json.Marshal(videos)
		checkErr("marshal json", err)
		w.Write(bs)
	})

	http.HandleFunc("/recently.json", func(w http.ResponseWriter, req *http.Request) {
		videos := []Video{}
		err := db.Select(&videos, `SELECT id, title, image FROM video 
			WHERE last_visit IS NOT NULL
			ORDER BY last_visit DESC LIMIT 50`)
		checkErr("select", err)
		bs, err := json.Marshal(videos)
		checkErr("marshal json", err)
		w.Write(bs)
	})

	http.HandleFunc("/go", func(w http.ResponseWriter, req *http.Request) {
		idStr := req.FormValue("id")
		id, err := strconv.Atoi(idStr)
		checkErr("parse id", err)
		db.MustExec(`UPDATE video SET 
			view = view + 1,
			last_visit = $2
			WHERE id = $1`, id, time.Now().Unix())
		http.Redirect(w, req, sp("http://www.bilibili.com/video/av%d", id), 307)
	})

	pt("starting http server\n")
	server := http.Server{
		Addr: ":19870",
	}
	http2.ConfigureServer(&server, nil)
	err = server.ListenAndServe()
	checkErr("start http server", err)
}

type Video struct {
	Title string `db:"title" json:"title"`
	Id    int    `db:"id" json:"id"`
	View  int    `db:"view" json:"view"`
	Image string `db:"image" json:"image"`
}

type VideoSorter []Video

func (s VideoSorter) Len() int      { return len(s) }
func (s VideoSorter) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s VideoSorter) Less(i, j int) bool {
	return s[i].Id > s[j].Id
}
