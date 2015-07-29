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

	collectFollowers := func() []int {
		uids := []int{}
		page := 1
	collect:
		jsn := get(sp("http://space.bilibili.com/ajax/friend/GetAttentionList?mid=19415&page=%d", page))
		var data struct {
			Status bool
			Data   struct {
				Pages   int
				Results int
				List    []struct {
					Fid int
				}
			}
		}
		err := json.NewDecoder(bytes.NewReader(jsn)).Decode(&data)
		ce(err, "decode following list")
		for _, u := range data.Data.List {
			uids = append(uids, u.Fid)
		}
		page++
		if page <= data.Data.Pages {
			goto collect
		}
		pt("following %d users\n", len(uids))
		return uids
	}

	collectVideo := func(uid int) {
		page := 1
	collect:
		jsn := get(sp(
			"http://space.bilibili.com/ajax/member/getSubmitVideos?mid=%d&page=%d",
			uid, page))
		var data struct {
			Status bool
			Data   struct {
				Pages int
				List  []struct {
					Aid   string
					Title string
					Pic   string
				}
			}
		}
		err := json.NewDecoder(bytes.NewReader(jsn)).Decode(&data)
		ce(err, "decode user video list")
		for _, v := range data.Data.List {
			_, err = db.Exec(`INSERT INTO video (id, title, image, added, uid)
					VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE id=id`,
				v.Aid, v.Title, v.Pic, time.Now().Unix(), uid)
		}
		page++
		if page <= data.Data.Pages && page < 30 {
			goto collect
		}
	}

	collectFollowed := func() {
		t0 := time.Now()
		users := collectFollowers()
		for _, uid := range users {
			collectVideo(uid)
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
					VALUES (?, ?, ?, ?) ON DUPLICATE KEY UPDATE id=id`, id, title, image, time.Now().Unix())
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
			WHERE id = ?`, time.Now().Unix(), id)
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
			WHERE id = ?`, time.Now().Unix(), id)
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
