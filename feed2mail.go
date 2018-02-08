package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	rss "github.com/jteeuwen/go-pkg-rss"

	"bytes"
	"net/smtp"
	"strings"

	"database/sql"
	"github.com/coopernurse/gorp"
	_ "github.com/mattn/go-sqlite3"
)

type Recipient struct {
	To string
}

type Smtp struct {
	Host       string
	Auth       bool
	From       string
	Sender     string
	Recipients []Recipient
	Debug      bool
}

type Feed struct {
	Uri string
}

type Sqlite3 struct {
	DbFile string
	Table  string
}

type Config struct {
	Smtp    Smtp
	Feeds   []Feed
	Sqlite3 Sqlite3
}

type DataStore struct {
	Id         int
	Url        string
	Update     string
	UpdateDate string
}

var wg sync.WaitGroup
var config Config

const utcformat = "2006-01-02T15:04:05Z"
const timeformat = "2006-01-02 15:04:05"

func main() {
	flag.Parse()

	arg := flag.Arg(0)
	log.Println("arg : ", arg)

	configFile := "feed2mail.json"
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		fmt.Println(p)
		f := filepath.Join(p, configFile)
		_, err := os.Stat(f)
		if err == nil {
			fmt.Println(f)
			configFile = f
			break
		}
	}
	jsonBuffer, err := ioutil.ReadFile(configFile)
	if err != nil {
		log.Println("ERROR: ", err)
		os.Exit(1)
	}
	//fmt.Println(string(jsonBuffer))

	err = json.Unmarshal(jsonBuffer, &config)
	if err != nil {
		log.Println("ERROR: ", err)
		os.Exit(1)
	}

	log.Printf("%#v\n", config)

	// This sets up a new feed and polls it for new channels/items.
	// Invoking it with 'go PollFeed(...)' to have the polling performed in a
	// separate goroutine, so we can poll mutiple feeds.
	//go PollFeed("http://blog.case.edu/news/feed.atom", 5, nil)

	log.Printf("%#v\n", config.Feeds)

	for _, feed := range config.Feeds {
		log.Printf("%#v\n", feed.Uri)
		wg.Add(1)
		go PollFeed(feed.Uri, 2)
		wg.Wait()
	}
}

func PollFeed(uri string, timeout int) {
	defer wg.Done()
	feed := rss.New(timeout, true, chanHandler, itemHandler)

	//for {
	if err := feed.Fetch(uri, nil); err != nil {
		fmt.Fprintf(os.Stderr, "[e] %s: %s\n", uri, err)
		return
	}

	//<-time.After(time.Duration(feed.SecondsTillUpdate() * 1e9))
	//}
}

func chanHandler(feed *rss.Feed, newchannels []*rss.Channel) {
	log.Printf("%d new channel(s) in %s\n", len(newchannels), feed.Url)
}

func itemHandler(feed *rss.Feed, ch *rss.Channel, newitems []*rss.Item) {
	log.Printf("%d new item(s) in %s\n", len(newitems), feed.Url)

	db, err := sql.Open("sqlite3", config.Sqlite3.DbFile)
	if err != nil {
		panic(err.Error())
	}
	dbmap := &gorp.DbMap{Db: db, Dialect: gorp.SqliteDialect{}}

	t := dbmap.AddTableWithName(DataStore{}, config.Sqlite3.Table).SetKeys(true, "Id")
	t.ColMap("Id").Rename("id")
	t.ColMap("Url").Rename("url")
	t.ColMap("Update").Rename("update")
	t.ColMap("UpdateDate").Rename("update_date")

	dbmap.CreateTablesIfNotExists()

	tx, _ := dbmap.Begin()
	for _, item := range newitems {
		update, _ := time.Parse(utcformat, item.Updated)
		now := time.Now()
		log.Printf("%s %s(%s) %s\n", item.Links[0].Href, update.Format(timeformat), item.Updated, now.Format(timeformat))
		count, _ := dbmap.SelectInt("select count(*) from feed2mail where `URL` = $1", item.Links[0].Href, update.Format(timeformat))
		if count == 0 {
			log.Printf("now %s \n", now.Format(timeformat))
			err = tx.Insert(&DataStore{0, item.Links[0].Href, update.Format(timeformat), now.Format(timeformat)})
			if err != nil {
				log.Println("Insert Error!")
				log.Println(err.Error())
			}
			sendmail(item)
		} else {
			id, _ := dbmap.SelectStr("select `ID` from feed2mail where `URL` = $1 and `UPDATE` < $2", item.Links[0].Href, update.Format(timeformat))
			log.Printf("id = %s", id)
			if id != "" {
				uid, _ := strconv.Atoi(id)
				count, err = tx.Update(&DataStore{uid, item.Links[0].Href, update.Format(timeformat), now.Format(timeformat)})
				if err != nil {
					log.Println("Update Error!")
					log.Println(err.Error())
				}
				sendmail(item)
			}
		}
	}
	tx.Commit()
}

func subjectSplit(title string, length int) []string {
	result := []string{}
	var buf bytes.Buffer

	for k, c := range strings.Split(title, "") {
		buf.WriteString(c)
		if k%length == length-1 {
			result = append(result, buf.String())
			buf.Reset()
		}
	}

	if buf.Len() > 0 {
		result = append(result, buf.String())
	}

	return result
}

func encodeSubject(subject string) string {
	var buf bytes.Buffer

	buf.WriteString("Subject:")
	for _, line := range subjectSplit(subject, 13) {
		buf.WriteString(" =?utf-8?B?")
		buf.WriteString(base64.StdEncoding.EncodeToString([]byte(line)))
		buf.WriteString("?=\r\n")
	}

	return buf.String()
}

func sendmail(item *rss.Item) {
	log.Println("title:", item.Title)
	if len(item.Links) > 0 {
		for i, link := range item.Links {
			log.Println("links", i, ":", link)
		}
	}
	log.Println("author:", item.Author)
	if len(item.Categories) > 0 {
		for i, category := range item.Categories {
			log.Println("categories", i, ":", category)
		}
	}

	jst := time.FixedZone("Asia/Tokyo", 9*60*60)

	pubdateUTC, _ := time.Parse(utcformat, item.PubDate)
	pubdate := pubdateUTC.In(jst)

	updateUTC, _ := time.Parse(utcformat, item.Updated)
	update := updateUTC.In(jst)

	log.Println("pubdate:", item.PubDate)
	log.Println("pubdate:", pubdate.Format(timeformat))
	log.Println("updated:", item.Updated)
	log.Println("updated:", update.Format(timeformat))
	log.Println("content:", item.Content.Text)
	fmt.Println("")

	return

	c, err := smtp.Dial(config.Smtp.Host)
	if err != nil {
		log.Println(err)
	}

	c.Mail(config.Smtp.From)
	for _, recipient := range config.Smtp.Recipients {
		c.Rcpt(recipient.To)
	}

	wc, err := c.Data()
	if err != nil {
		log.Println("Data() ", err)
	}
	defer wc.Close()

	var message bytes.Buffer
	message.WriteString("Title: ")
	message.WriteString(item.Title)
	message.WriteString("<br />\r\n")
	message.WriteString("Auther: ")
	message.WriteString(item.Author.Name)
	if item.Author.Email != "" {
		message.WriteString(" <")
		message.WriteString(item.Author.Email)
		message.WriteString("> ")
	}
	message.WriteString("<br />\r\n")
	if item.PubDate != "" {
		message.WriteString("Publish Date: ")
		message.WriteString(pubdate.Format(timeformat))
		message.WriteString("<br />\r\n")
	}
	message.WriteString("Update Date: ")
	message.WriteString(update.Format(timeformat))
	message.WriteString("<br />\r\n")
	if len(item.Links) > 0 {
		for _, link := range item.Links {
			message.WriteString("URL: ")
			message.WriteString(link.Href)
			message.WriteString("<br />\r\n")
		}
	}
	if len(item.Categories) > 0 {
		for _, category := range item.Categories {
			message.WriteString("Category: ")
			message.WriteString(category.Text)
			message.WriteString("<br />\r\n")
		}
	}
	message.WriteString("---<br />\r\n")
	message.WriteString(item.Content.Text)

	buf := bytes.NewBufferString("")
	for i, recipient := range config.Smtp.Recipients {
		if i == 0 {
			buf.WriteString("To: " + recipient.To)
		} else {
			buf.WriteString("Bcc: " + recipient.To)
		}
		buf.WriteString("\r\n")
	}
	buf.WriteString(encodeSubject("feed2mail: " + item.Title))
	buf.WriteString("MIME-Version: 1.0\r\n")
	//buf.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	buf.WriteString("Content-Type: text/html; charset=\"utf-8\"\r\n")
	buf.WriteString("Content-Transfer-Encoding: base64\r\n")
	buf.WriteString("\r\n")
	buf.WriteString(base64.StdEncoding.EncodeToString(message.Bytes()))
	if _, err = buf.WriteTo(wc); err != nil {
		log.Println("WriteTo() ", err)
	}

	c.Quit()
}
