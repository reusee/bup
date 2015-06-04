package main

import (
	"bytes"
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

	"github.com/PuerkitoBio/goquery"
	"github.com/bradfitz/http2"
	"github.com/jackc/pgx"
	_ "github.com/jackc/pgx/stdlib"
	"github.com/jmoiron/sqlx"
)

var (
	pt          = fmt.Printf
	sp          = fmt.Sprintf
	logFilePath = flag.String("log", "", "log file path")
)

func main() {
	flag.Parse()
	defer func() {
		if p := recover(); p != nil && *logFilePath != "" {
			ioutil.WriteFile(*logFilePath, []byte(fmt.Sprintf("%v", p)), 0644)
		}
	}()

	checkErr := func(msg string, err error) {
		if err != nil {
			panic(sp("%s error: %v", msg, err))
		}
	}

	db, err := sqlx.Connect("pgx", "postgres://reus@localhost/bilibili")
	checkErr("connect to psql", err)
	err = db.Ping()
	checkErr("ping database", err)

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
				checkErr(sp("get %s", url), err)
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
				checkErr(sp("read %s", url), err)
			}
		}
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
			_, err = db.Exec(`INSERT INTO video (id, title, image, added, uid) 
				VALUES ($1, $2, $3, $4, $5)`, id, title, image, time.Now().Unix(), uid)
			if err != nil {
				if err.(pgx.PgError).Code == "23505" { // dup
					dup++
				} else {
					panic(err)
				}
			}
			count++
		})
		return count, dup
	}

	collectFollowed := func() {
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
				_, err = db.Exec(`INSERT INTO video (id, title, image, added)
					VALUES ($1, $2, $3, $4)`, id, title, image, time.Now().Unix())
				if err != nil {
					if err.(pgx.PgError).Code == "23505" { // dup
					} else {
						panic(err)
					}
				}
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

	root := "./react"
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
			ORDER BY id DESC, added DESC LIMIT 50`)
		checkErr("select", err)
		bs, err := json.Marshal(videos)
		checkErr("marshal json", err)
		w.Write(bs)
	})

	http.HandleFunc("/recently.json", func(w http.ResponseWriter, req *http.Request) {
		videos := []Video{}
		err := db.Select(&videos, `SELECT id, title, image FROM video 
			WHERE last_visit IS NOT NULL
			ORDER BY last_visit DESC LIMIT 20`)
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

	http.HandleFunc("/mark", func(w http.ResponseWriter, req *http.Request) {
		idStr := req.FormValue("id")
		id, err := strconv.Atoi(idStr)
		checkErr("parse id", err)
		db.MustExec(`UPDATE video SET 
			view = view + 1,
			last_visit = $2
			WHERE id = $1`, id, time.Now().Unix())
		bs, err := json.Marshal(struct{ Ok bool }{true})
		checkErr("marshal json", err)
		w.Write(bs)
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
