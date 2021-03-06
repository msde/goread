/*
 * Copyright (c) 2013 Matt Jibson <matt.jibson@gmail.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package goread

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mjibson/goon"
	"github.com/msde/go-charset/charset"

	"google.golang.org/appengine"
	"google.golang.org/appengine/blobstore"
	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/taskqueue"
	"google.golang.org/appengine/urlfetch"
)

func taskNameShouldEscape(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return false
	case '-' == c:
		return false
	}
	// We use underscore to escape other characters, so escape it.
	return true
}

/*
Hex-escape characters that are not allowed in appengine task names.
This is like URL hex-escaping, but using '_' instead of '%'.
This means '_' must also be escaped.

https://cloud.google.com/appengine/docs/go/taskqueue#Go_Task_names
[0-9a-zA-Z\-\_]{1,500}

This function does not try to enforce length.

The golang implementation of taskqueue does not enforce any of these rules,
but it seems prudent to comply unless Google changes the documentation.
*/
func taskNameEscape(s string) string {
	// Or we could trade memory for cycles and assume hexCount = len(s)
	hexCount := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if taskNameShouldEscape(c) {
			hexCount++
		}
	}
	if hexCount == 0 {
		return s
	}

	t := make([]byte, len(s)+2*hexCount)
	j := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case taskNameShouldEscape(c):
			t[j] = '_'
			t[j+1] = "0123456789ABCDEF"[c>>4]
			t[j+2] = "0123456789ABCDEF"[c&15]
			j += 3
		default:
			t[j] = s[i]
			j++
		}
	}
	return string(t)
}

func ImportOpmlTask(w http.ResponseWriter, r *http.Request) {
	c := r.Context()
	gn := goon.FromContext(c)
	userid := r.FormValue("user")
	bk := r.FormValue("key")
	del := func() {
		blobstore.Delete(c, appengine.BlobKey(bk))
	}

	var skip int
	if s, err := strconv.Atoi(r.FormValue("skip")); err == nil {
		skip = s
	}
	log.Debugf(c, "reader import for %v, skip %v", userid, skip)

	d := xml.NewDecoder(blobstore.NewReader(c, appengine.BlobKey(bk)))
	d.CharsetReader = charset.NewReader
	d.Strict = false
	opml := Opml{}
	err := d.Decode(&opml)
	if err != nil {
		log.Warningf(c, "gob decode failed: %v", err.Error())
		del()
		return
	}

	remaining := skip
	var userOpml []*OpmlOutline
	var proc func(label string, outlines []*OpmlOutline)
	proc = func(label string, outlines []*OpmlOutline) {
		for _, o := range outlines {
			if o.Title == "" {
				o.Title = o.Text
			}
			if o.XmlUrl != "" {
				if remaining > 0 {
					remaining--
				} else if len(userOpml) < IMPORT_LIMIT {
					userOpml = append(userOpml, &OpmlOutline{
						Title:   label,
						Outline: []*OpmlOutline{o},
					})
				}
			}

			if o.Title != "" && len(o.Outline) > 0 {
				proc(o.Title, o.Outline)
			}
		}
	}

	proc("", opml.Outline)

	// todo: refactor below with similar from ImportReaderTask
	wg := sync.WaitGroup{}
	wg.Add(len(userOpml))
	for i := range userOpml {
		go func(i int) {
			o := userOpml[i].Outline[0]
			if err := addFeed(c, userid, userOpml[i]); err != nil {
				log.Warningf(c, "opml import error: %v", err.Error())
				// todo: do something here?
			}
			log.Debugf(c, "opml import: %s, %s", o.Title, o.XmlUrl)
			wg.Done()
		}(i)
	}
	wg.Wait()

	ud := UserData{Id: "data", Parent: gn.Key(&User{Id: userid})}
	if err := gn.RunInTransaction(func(gn *goon.Goon) error {
		gn.Get(&ud)
		if err := mergeUserOpml(c, &ud, userOpml...); err != nil {
			return err
		}
		_, err := gn.Put(&ud)
		return err
	}, nil); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Errorf(c, "ude update error: %v", err.Error())
		return
	}

	if len(userOpml) == IMPORT_LIMIT {
		task := taskqueue.NewPOSTTask(routeUrl("import-opml-task"), url.Values{
			"key":  {bk},
			"user": {userid},
			"skip": {strconv.Itoa(skip + IMPORT_LIMIT)},
		})
		taskqueue.Add(c, task, "import-reader")
	} else {
		log.Infof(c, "opml import done: %v", userid)
		del()
	}
}

const IMPORT_LIMIT = 10

func SubscribeCallback(w http.ResponseWriter, r *http.Request) {
	c := r.Context()
	gn := goon.FromContext(c)
	furl := r.FormValue("feed")
	b, _ := base64.URLEncoding.DecodeString(furl)
	f := Feed{Url: string(b)}
	log.Infof(c, "url: %v", f.Url)
	if err := gn.Get(&f); err != nil {
		http.Error(w, "", http.StatusNotFound)
		return
	}
	fk := gn.Key(&f)
	if r.Method == "GET" {
		if f.NotViewed() || r.FormValue("hub.mode") != "subscribe" || r.FormValue("hub.topic") != f.Url {
			http.Error(w, "", http.StatusNotFound)
			return
		}
		w.Write([]byte(r.FormValue("hub.challenge")))
		i, _ := strconv.Atoi(r.FormValue("hub.lease_seconds"))
		f.Subscribed = time.Now().Add(time.Second * time.Duration(i))
		gn.Put(&f)
		log.Debugf(c, "subscribed: %v - %v - %v", fk, f.Url, f.Subscribed)
		return
	} else if !f.NotViewed() {
		log.Infof(c, "push: %v - %v", fk, f.Url)
		defer r.Body.Close()
		b, _ := ioutil.ReadAll(r.Body)
		nf, ss, err := ParseFeed(c, r.Header.Get("Content-Type"), f.Url, f.Url, b)
		if err != nil {
			log.Errorf(c, "parse error: %v", err)
			return
		}
		if err := updateFeed(c, f.Url, nf, ss, false, true, false); err != nil {
			log.Errorf(c, "push error: %v", err)
		}
	} else {
		log.Infof(c, "not viewed - %v", fk)
	}
}

// Task used to subscribe a feed to push.
func SubscribeFeed(w http.ResponseWriter, r *http.Request) {
	c := r.Context()
	start := time.Now()
	gn := goon.FromContext(c)
	f := Feed{Url: r.FormValue("feed")}
	fk := gn.Key(&f)
	s := ""
	defer func() {
		log.Infof(c, "SubscribeFeed - %v - start %s - f.sub %s - %s",
			fk, start.String(), f.Subscribed.String(), s)
	}()
	if err := gn.Get(&f); err != nil {
		log.Errorf(c, "%v: %v", err, f.Url)
		serveError(w, err)
		s += "err"
		return
	} else if f.IsSubscribed() {
		s += "is subscribed"
		return
	}
	u := url.Values{}
	u.Add("hub.callback", f.PubSubURL())
	u.Add("hub.mode", "subscribe")
	u.Add("hub.verify", "sync")
	fu, _ := url.Parse(f.Url)
	fu.Fragment = ""
	u.Add("hub.topic", fu.String())
	req, err := http.NewRequest("POST", f.Hub, strings.NewReader(u.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cl := &http.Client{
		Transport: &urlfetch.Transport{
			Context: c,
		},
	}
	resp, err := cl.Do(req)
	if err != nil {
		log.Errorf(c, "req error: %v", err)
	} else if resp.StatusCode != http.StatusNoContent {
		f.Subscribed = time.Now().Add(time.Hour * 48)
		gn.Put(&f)
		if resp.StatusCode != http.StatusConflict {
			log.Errorf(c, "resp: %v - %v", f.Url, resp.Status)
			log.Errorf(c, "%s", resp.Body)
		}
		s += "resp err"
	} else {
		log.Infof(c, "subscribed: %v", f.Url)
		s += "success"
	}
}

// cleanup "L"=Log entities, which otherwise fill datastore quota
// AFAICT they were only ever read back in the admin viewer....
// This runs as a cron task
func DatastoreCleanup(w http.ResponseWriter, r *http.Request) {
	// add timeout to context?
	c := appengine.NewContext(r)
	g := goon.FromContext(c)
	limit := 2000
	q := datastore.NewQuery(g.Kind(&Log{})).Limit(limit).KeysOnly()
	log.Debugf(c, "DatastoreCleanup: limit %v", limit)
	keys, err := q.GetAll(c, nil)
	if err != nil {
		log.Criticalf(c, "err: %v", err)
		return
	}
	log.Infof(c, "DatastoreCleanup: %v/%v", len(keys), limit)
	if len(keys) == 0 {
		return
	}
	err = g.DeleteMulti(keys)
	if err != nil {
		log.Criticalf(c, "err: %v", err)
	}
}

func UpdateFeeds(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	now := time.Now()
	limit := 10 * 60 * 2 // 10-Hz queue, 2-min cron
	q := datastore.NewQuery("F").Filter("n <=", now).Limit(limit)
	tctx, cancel := context.WithTimeout(c, time.Minute)
	defer cancel()
	it := q.Run(tctx)
	tc := make(chan *taskqueue.Task)
	done := make(chan bool)
	i := 0
	u := routeUrl("update-feed")
	var feed Feed
	var id string

	go taskSender(c, "update-feed", tc, done)
	for {
		k, err := it.Next(&feed)
		if err == datastore.Done {
			break
		}
		if err != nil {
			log.Errorf(c, "next error: %v", err.Error())
			break
		}
		id = k.StringID()
		// To guard against queuing duplicate feeds,
		// use the feed id (URL) as the task name.
		// https://cloud.google.com/appengine/docs/go/taskqueue#Go_Task_names
		// This is not an absolute guarantee, but should help.
		newTask := taskqueue.NewPOSTTask(u, url.Values{"feed": {id}})
		// Include the NextUpdate time because task names endure 7D or so.
		// The URL is hex-escaped but hopefully still human-readable.
		newTask.Name = fmt.Sprintf("%v_%v",
			feed.NextUpdate.UTC().Format("2006-01-02T15-04-05Z07-00"),
			taskNameEscape(id))
		log.Debugf(c, "queuing feed %v", newTask.Name)
		tc <- newTask
		i++
	}
	close(tc)
	<-done
	log.Infof(c, "updating %d feeds", i)
}

func fetchFeed(c context.Context, origUrl, fetchUrl string) (*Feed, []*Story, error) {
	u, err := url.Parse(fetchUrl)
	if err != nil {
		return nil, nil, err
	}
	if u.Host == "" {
		u.Host = u.Path
		u.Path = ""
	}
	const clURL = "craigslist.org"
	if strings.HasSuffix(u.Host, clURL) || u.Host == clURL {
		return nil, nil, fmt.Errorf("Craigslist blocks our server host: not possible to subscribe")
	}
	if u.Scheme == "" {
		u.Scheme = "http"
		origUrl = u.String()
		fetchUrl = origUrl
		if origUrl == "" {
			return nil, nil, fmt.Errorf("bad URL")
		}
	}

	cl := &http.Client{
		Transport: &urlfetch.Transport{
			Context: c,
		},
	}
	if resp, err := cl.Get(fetchUrl); err == nil && resp.StatusCode == http.StatusOK {
		const sz = 1 << 21
		reader := &io.LimitedReader{R: resp.Body, N: sz}
		defer resp.Body.Close()
		b, err := ioutil.ReadAll(reader)
		if err != nil {
			return nil, nil, err
		}
		if reader.N == 0 {
			return nil, nil, fmt.Errorf("feed larger than %d bytes", sz)
		}
		if autoUrl, err := Autodiscover(b); err == nil && origUrl == fetchUrl {
			if autoU, err := url.Parse(autoUrl); err == nil {
				if autoU.Scheme == "" {
					autoU.Scheme = u.Scheme
				}
				if autoU.Host == "" {
					autoU.Host = u.Host
				}
				autoUrl = autoU.String()
			}
			if autoUrl != fetchUrl {
				return fetchFeed(c, origUrl, autoUrl)
			}
		}
		return ParseFeed(c, resp.Header.Get("Content-Type"), origUrl, fetchUrl, b)
	} else if err != nil {
		log.Warningf(c, "fetch feed error: %v", err)
		return nil, nil, fmt.Errorf("Could not fetch feed")
	} else {
		log.Warningf(c, "fetch feed error: status code: %s %s",
			resp.Status, resp.Body)
		return nil, nil, fmt.Errorf("Bad response code from server")
	}
}

func updateFeed(c context.Context, url string, feed *Feed, stories []*Story, updateAll, fromSub, updateLast bool) error {
	// TODO this used to have a 1-min timeout
	// but I'm confused about datastore context vs goon
	// godoc.org/cloud.google.com/go/datastore
	gn := goon.FromContext(c)
	f := Feed{Url: url}
	if err := gn.Get(&f); err != nil {
		return fmt.Errorf("feed not found: %s", url)
	}
	log.Debugf(c, "feed update: %v", gn.Key(&f))

	// Compare the feed's listed update to the story's update.
	// Note: these may not be accurate, hence, only compare them to each other,
	// since they should have the same relative error.
	storyDate := f.Updated

	hasUpdated := !feed.Updated.IsZero()
	isFeedUpdated := f.Updated.Equal(feed.Updated)
	if !hasUpdated {
		feed.Updated = f.Updated
	}
	feed.Date = f.Date
	feed.Average = f.Average
	feed.LastViewed = f.LastViewed
	f = *feed
	if updateLast {
		f.LastViewed = time.Now()
	}

	if hasUpdated && isFeedUpdated && !updateAll && !fromSub {
		log.Infof(c, "feed %s already updated to %v, putting", url, feed.Updated)
		f.Updated = time.Now()
		scheduleNextUpdate(c, &f)
		gn.Put(&f)
		return nil
	}

	log.Debugf(c, "hasUpdate: %v, isFeedUpdated: %v, storyDate: %v, stories: %v", hasUpdated, isFeedUpdated, storyDate, len(stories))
	puts := []interface{}{&f}

	// find non existant stories
	fk := gn.Key(&f)
	getStories := make([]*Story, len(stories))
	for i, s := range stories {
		getStories[i] = &Story{Id: s.Id, Parent: fk}
	}
	err := gn.GetMulti(getStories)
	if _, ok := err.(appengine.MultiError); err != nil && !ok {
		log.Errorf(c, "GetMulti error: %v", err)
		return err
	}
	var updateStories []*Story
	for i, s := range getStories {
		if goon.NotFound(err, i) {
			updateStories = append(updateStories, stories[i])
		} else if (!stories[i].Updated.IsZero() && !stories[i].Updated.Equal(s.Updated)) || updateAll {
			if !s.Created.IsZero() {
				stories[i].Created = s.Created
			}
			if !s.Published.IsZero() {
				stories[i].Published = s.Published
			}
			updateStories = append(updateStories, stories[i])
		}
	}
	log.Debugf(c, "%v update stories", len(updateStories))

	for _, s := range updateStories {
		puts = append(puts, s)
		sc := StoryContent{
			Id:     1,
			Parent: gn.Key(s),
		}
		buf := &bytes.Buffer{}
		if gz, err := gzip.NewWriterLevel(buf, gzip.BestCompression); err == nil {
			gz.Write([]byte(s.content))
			gz.Close()
			sc.Compressed = buf.Bytes()
		}
		if len(sc.Compressed) == 0 {
			sc.Content = s.content
		}
		if _, err := gn.Put(&sc); err != nil {
			log.Errorf(c, "put sc err: %v", err)
			return err
		}
	}

	log.Debugf(c, "putting %v entities", len(puts))
	if len(puts) > 1 {
		updateAverage(&f, f.Date, len(puts)-1)
		f.Date = time.Now()
		if !hasUpdated {
			f.Updated = f.Date
		}
	}
	scheduleNextUpdate(c, &f)
	if fromSub {
		wait := time.Now().Add(time.Hour * 6)
		if f.NextUpdate.Before(wait) {
			f.NextUpdate = wait
		}
	}
	delay := f.NextUpdate.Sub(time.Now())
	log.Infof(c, "next update scheduled for %v from now", delay-delay%time.Second)
	_, err = gn.PutMulti(puts)
	if err != nil {
		log.Errorf(c, "update put err: %v", err)
	}
	return err
}

func UpdateFeed(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	gn := goon.FromContext(c)
	url := r.FormValue("feed")
	if url == "" {
		log.Errorf(c, "empty update feed")
		return
	}
	log.Debugf(c, "update feed %s", url)
	last := len(r.FormValue("last")) > 0
	f := Feed{Url: url}
	s := ""
	defer func() {
		log.Debugf(c, "UpdateFeed:%v - %s", gn.Key(&f), s)
	}()
	if err := gn.Get(&f); err == datastore.ErrNoSuchEntity {
		log.Errorf(c, "no such entity - "+url)
		s += "NSE"
		return
	} else if err != nil {
		s += "err - " + err.Error()
		return
	} else if last {
		// noop
	} else if time.Now().Before(f.NextUpdate) {
		log.Errorf(c, "feed %v already updated: %v", url, f.NextUpdate)
		s += "already updated"
		return
	}

	feedError := func(err error) {
		s += "feed err - " + err.Error()
		f.Errors++
		v := f.Errors + 1
		const max = 24 * 7
		if v > max {
			v = max
		} else if f.Errors == 1 {
			v = 0
		}
		f.NextUpdate = time.Now().Add(time.Hour * time.Duration(v))
		gn.Put(&f)
		log.Warningf(c, "error with %v (%v), bump next update to %v, %v", url, f.Errors, f.NextUpdate, err)
	}

	if feed, stories, err := fetchFeed(c, f.Url, f.Url); err == nil {
		if err := updateFeed(c, f.Url, feed, stories, false, false, last); err != nil {
			feedError(err)
		} else {
			s += "success"
		}
	} else {
		feedError(err)
	}
	f.Subscribe(c)
}

func UpdateFeedLast(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	gn := goon.FromContext(c)
	url := r.FormValue("feed")
	log.Debugf(c, "update feed last %s", url)
	f := Feed{Url: url}
	if err := gn.Get(&f); err != nil {
		return
	}
	f.LastViewed = time.Now()
	gn.Put(&f)
}

func DeleteBlobs(c context.Context, w http.ResponseWriter, r *http.Request) {
	tctx, cancel := context.WithTimeout(c, time.Minute)
	defer cancel()
	q := datastore.NewQuery("__BlobInfo__").KeysOnly()
	it := q.Run(tctx)
	wg := sync.WaitGroup{}
	something := false
	for _i := 0; _i < 20; _i++ {
		var bk []appengine.BlobKey
		for i := 0; i < 1000; i++ {
			k, err := it.Next(nil)
			if err == datastore.Done {
				break
			} else if err != nil {
				log.Errorf(c, "err: %v", err)
				continue
			}
			bk = append(bk, appengine.BlobKey(k.StringID()))
		}
		if len(bk) == 0 {
			break
		}
		go func(bk []appengine.BlobKey) {
			something = true
			log.Errorf(c, "deleteing %v blobs", len(bk))
			err := blobstore.DeleteMulti(tctx, bk)
			if err != nil {
				log.Errorf(c, "blobstore delete err: %v", err)
			}
			wg.Done()
		}(bk)
		wg.Add(1)
	}
	wg.Wait()
	if something {
		taskqueue.Add(c, taskqueue.NewPOSTTask("/tasks/delete-blobs", nil), "")
	}
}

func DeleteOldFeeds(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	gn := goon.FromContext(c)
	q := datastore.NewQuery(gn.Kind(&Feed{})).Filter("n=", timeMax).KeysOnly()
	if cur, err := datastore.DecodeCursor(r.FormValue("c")); err == nil {
		q = q.Start(cur)
	}
	tctx, cancel := context.WithTimeout(c, time.Minute)
	defer cancel()
	it := q.Run(tctx)
	done := false
	var tasks []*taskqueue.Task
	for i := 0; i < 10000 && len(tasks) < 100; i++ {
		k, err := it.Next(nil)
		if err == datastore.Done {
			log.Criticalf(c, "done")
			done = true
			break
		} else if err != nil {
			log.Errorf(c, "err: %v", err)
			continue
		}
		values := make(url.Values)
		values.Add("f", k.StringID())
		tasks = append(tasks, taskqueue.NewPOSTTask("/tasks/delete-old-feed", values))
	}
	if len(tasks) > 0 {
		log.Errorf(c, "deleting %v feeds", len(tasks))
		if _, err := taskqueue.AddMulti(c, tasks, ""); err != nil {
			log.Errorf(c, "err: %v", err)
		}
	}
	if !done {
		if cur, err := it.Cursor(); err == nil {
			values := make(url.Values)
			values.Add("c", cur.String())
			taskqueue.Add(c, taskqueue.NewPOSTTask("/tasks/delete-old-feeds", values), "")
		} else {
			log.Errorf(c, "err: %v", err)
		}
	}
}

func DeleteOldFeed(w http.ResponseWriter, r *http.Request) {
	// TODO this used to have a 1-min timeout
	// but I'm confused about datastore context vs goon
	// godoc.org/cloud.google.com/go/datastore
	c := appengine.NewContext(r)
	g := goon.FromContext(c)
	oldDate := time.Now().Add(-time.Hour * 24 * 90)
	feed := Feed{Url: r.FormValue("f")}
	if err := g.Get(&feed); err != nil {
		log.Criticalf(c, "err: %v", err)
		return
	}
	if feed.LastViewed.After(oldDate) {
		return
	}
	q := datastore.NewQuery(g.Kind(&Story{})).Ancestor(g.Key(&feed)).KeysOnly()
	tctx, cancel := context.WithTimeout(c, time.Minute)
	defer cancel()
	keys, err := q.GetAll(tctx, nil)
	if err != nil {
		log.Criticalf(c, "err: %v", err)
		return
	}
	q = datastore.NewQuery(g.Kind(&StoryContent{})).Ancestor(g.Key(&feed)).KeysOnly()
	sckeys, err := q.GetAll(tctx, nil)
	if err != nil {
		log.Criticalf(c, "err: %v", err)
		return
	}
	keys = append(keys, sckeys...)
	log.Infof(c, "delete: %v - %v", feed.Url, len(keys))
	feed.NextUpdate = timeMax.Add(time.Hour)
	if _, err := g.Put(&feed); err != nil {
		log.Criticalf(c, "put err: %v", err)
	}
	if len(keys) == 0 {
		return
	}
	err = g.DeleteMulti(keys)
	if err != nil {
		log.Criticalf(c, "err: %v", err)
	}
}
