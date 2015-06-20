package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "net/http/pprof"

	_ "github.com/go-sql-driver/mysql"
	"github.com/reusee/catch"

	"github.com/PuerkitoBio/goquery"
	"github.com/bradfitz/http2"
)

var (
	pt          = fmt.Printf
	sp          = fmt.Sprintf
	logFilePath = flag.String("log", "", "log file path")
	ce          = catch.PkgChecker("bilibili")
	ct          = catch.Catch
)

func main() {
	flag.Parse()
	defer func() {
		if p := recover(); p != nil && *logFilePath != "" {
			ioutil.WriteFile(*logFilePath, []byte(fmt.Sprintf("%v", p)), 0644)
		}
	}()

	db, err := sql.Open("mysql", "root@unix(/var/run/mysqld/mysqld.sock)/bilibili?tokudb_commit_sync=off")
	ce(err, "connect to db")

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS video (
		id INTEGER PRIMARY KEY,
		title TEXT,
		view INTEGER NOT NULL DEFAULT 0,
		last_visit INTEGER,
		image TEXT,
		added INTEGER DEFAULT 0,
		uid CHAR(64)
		) engine=TokuDB`)
	ce(err, "create table")

	get := func(url string) []byte {
		retryCount := 8
	retry:
		client := http.Client{
			Timeout: time.Second * 16,
		}
		pt("get %s\n", url)
		resp, err := client.Get(url)
		if err != nil {
			if retryCount > 0 {
				retryCount--
				time.Sleep(time.Second * 3)
				goto retry
			} else {
				ce(err, sp("get %s", url))
			}
		}
		defer resp.Body.Close()
		content, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			if retryCount > 0 {
				retryCount--
				time.Sleep(time.Second * 3)
				goto retry
			} else {
				ce(err, sp("read %s", url))
			}
		}
		return content
	}

	bytesToDoc := func(bs []byte) *goquery.Document {
		doc, err := goquery.NewDocumentFromReader(bytes.NewReader(bs))
		ce(err, "get document")
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
					panic("invalid entry")
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

	collectVideo := func(uid string, page int) int {
		url := sp("http://space.bilibili.com/space?uid=%s&page=%d", uid, page)
		doc := bytesToDoc(get(url))
		entries := doc.Find("div.main_list ul li")
		count := 0
		entries.Each(func(n int, se *goquery.Selection) {
			titleSe := se.Find("a.title")
			title := titleSe.Text()
			href, ok := titleSe.Attr("href")
			if !ok || len(title) == 0 {
				panic("invalid entry")
			}
			idStr := href[strings.LastIndex(href, "av")+2:]
			idStr = idStr[:len(idStr)-1]
			id, err := strconv.Atoi(idStr)
			imgSe := se.Find("a img")
			image, ok := imgSe.Attr("src")
			if !ok {
				panic("no image")
			}
			_, err = db.Exec(`INSERT IGNORE INTO video (id, title, image, added, uid) 
				VALUES (?, ?, ?, ?, ?)`, id, title, image, time.Now().Unix(), uid)
			ce(err, "insert")
			count++
		})
		return count
	}

	collectFollowed := func() {
		t0 := time.Now()
		users := collectFollowers()
		for _, uid := range users {
			page := 1
			for page < 50 {
				count := collectVideo(uid, page)
				if count == 0 {
					break
				}
				page++
			}
		}
		pt("collected in %v\n", time.Now().Sub(t0))
	}

	collectHottest := func() {
		urls := []string{
			"http://www.bilibili.com/video/bagumi_offical_1.html#!order=hot&page=1", // 官方延伸所有新投稿
		}
		end := time.Now()
		start := end.AddDate(0, 0, -7)
		rangeStr := sp("%4d-%02d-%02d~%4d-%02d-%02d", start.Year(), start.Month(), start.Day(),
			end.Year(), end.Month(), end.Day())
		urls = append(urls, sp("http://www.bilibili.com/list/damku-29-1-%s.html", rangeStr)) // 三次元音乐弹幕排序
		urls = append(urls, sp("http://www.bilibili.com/list/damku-17-1-%s.html", rangeStr)) // 单机联机弹幕排序
		urls = append(urls, sp("http://www.bilibili.com/list/damku-37-1-%s.html", rangeStr)) // 纪录片弹幕排序
		urls = append(urls, sp("http://www.bilibili.com/list/damku-51-1-%s.html", rangeStr)) // 动画资讯弹幕排序
		urls = append(urls, sp("http://www.bilibili.com/list/damku-98-1-%s.html", rangeStr)) // 机械弹幕排序
		for _, url := range urls {
			doc := bytesToDoc(get(url))
			entries := doc.Find("ul.vd-list li")
			entries.Each(func(i int, se *goquery.Selection) {
				titleSe := se.Find("a.title")
				title := titleSe.Text()
				href, ok := titleSe.Attr("href")
				if !ok || len(title) == 0 {
					panic(sp("invalid entry in %s # %d", url, i))
				}
				idStr := href[strings.LastIndex(href, "av")+2:]
				idStr = idStr[:len(idStr)-1]
				id, err := strconv.Atoi(idStr)
				if err != nil {
					panic(sp("invalid entry in %s # %d", url, i))
				}
				imgSe := se.Find("a img")
				image, ok := imgSe.Attr("src")
				if !ok {
					panic(sp("invalid entry in %s # %d", url, i))
				}
				//pt("%d %s %s\n", id, title, image)
				_, err = db.Exec(`INSERT IGNORE INTO video (id, title, image, added)
					VALUES (?, ?, ?, ?)`, id, title, image, time.Now().Unix())
				ce(err, "insert")
			})
		}
	}

	go func() {
		for {
			func() {
				defer recover()
				collectHottest()
				collectFollowed()
			}()
			time.Sleep(time.Minute * 5)
		}
	}()

	root := "./web"
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	root, err = filepath.Abs(root)
	ce(err, "get web root dir")
	pt("web root %s\n", root)
	http.Handle("/", http.FileServer(http.Dir(root)))

	http.HandleFunc("/newest.json", func(w http.ResponseWriter, req *http.Request) {
		videos := []Video{}
		rows, err := db.Query(`SELECT id, title, image FROM video 
			WHERE view < 1
			ORDER BY id DESC, added DESC LIMIT 50`)
		ce(err, "query")
		for rows.Next() {
			var video Video
			ce(rows.Scan(&video.Id, &video.Title, &video.Image), "scan")
			videos = append(videos, video)
		}
		ce(rows.Err(), "rows")
		bs, err := json.Marshal(videos)
		ce(err, "marshal")
		w.Write(bs)
	})

	http.HandleFunc("/recently.json", func(w http.ResponseWriter, req *http.Request) {
		videos := []Video{}
		rows, err := db.Query(`SELECT id, title, image FROM video 
			WHERE last_visit IS NOT NULL
			ORDER BY last_visit DESC LIMIT 20`)
		ce(err, "query")
		for rows.Next() {
			var video Video
			ce(rows.Scan(&video.Id, &video.Title, &video.Image), "scan")
			videos = append(videos, video)
		}
		ce(rows.Err(), "rows")
		bs, err := json.Marshal(videos)
		ce(err, "marshal")
		w.Write(bs)
	})

	http.HandleFunc("/go", func(w http.ResponseWriter, req *http.Request) {
		idStr := req.FormValue("id")
		id, err := strconv.Atoi(idStr)
		ce(err, "parse id")
		_, err = db.Exec(`UPDATE video SET
			view = view + 1,
			last_visit = ?
			WHERE id = ?`, id, time.Now().Unix())
		ce(err, "update")
		http.Redirect(w, req, sp("http://www.bilibili.com/video/av%d", id), 307)
	})

	http.HandleFunc("/mark", func(w http.ResponseWriter, req *http.Request) {
		idStr := req.FormValue("id")
		id, err := strconv.Atoi(idStr)
		ce(err, "parse id")
		_, err = db.Exec(`UPDATE video SET 
			view = view + 1,
			last_visit = ?
			WHERE id = ?`, id, time.Now().Unix())
		ce(err, "update")
		bs, err := json.Marshal(struct{ Ok bool }{true})
		ce(err, "marshal json")
		w.Write(bs)
	})

	pt("starting http server\n")
	server := http.Server{
		Addr: ":19870",
	}
	http2.ConfigureServer(&server, nil)
	err = server.ListenAndServe()
	ce(err, "start http server")
}

type Video struct {
	Title string `db:"title" json:"title"`
	Id    int    `db:"id" json:"id"`
	View  int    `db:"view" json:"view"`
	Image string `db:"image" json:"image"`
}
