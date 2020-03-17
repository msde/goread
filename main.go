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
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	_log "log"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/mux"
	"github.com/mjibson/goon"

	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/log"
)

var router = new(mux.Router)
var templates *template.Template

func init() {
	var err error
	if templates, err = template.New("").Funcs(funcs).
		ParseFiles(
			"templates/base.html",
			"templates/admin-all-feeds.html",
			"templates/admin-date-formats.html",
			"templates/admin-feed.html",
			"templates/admin-stats.html",
			"templates/admin-user.html",
		); err != nil {
		_log.Fatal(err)
	}
}

// TODO this looks tricky to port to go111
// might need a rewrite....
// use gorilla mux middleware to supply context?
func RegisterHandlers(r *mux.Router) {
	router = r
	router.HandleFunc("/", Main).Name("main")
	router.HandleFunc("/login/google", LoginGoogle).Name("login-google")
	router.HandleFunc("/logout", Logout).Name("logout")
	router.HandleFunc("/push", SubscribeCallback).Name("subscribe-callback")
	router.HandleFunc("/tasks/datastore-cleanup", DatastoreCleanup).Name("datastore-cleanup")
	router.HandleFunc("/tasks/import-opml", ImportOpmlTask).Name("import-opml-task")
	router.HandleFunc("/tasks/subscribe-feed", SubscribeFeed).Name("subscribe-feed")
	router.HandleFunc("/tasks/update-feed-last", UpdateFeedLast).Name("update-feed-last")
	router.HandleFunc("/tasks/update-feed-manual", UpdateFeed).Name("update-feed-manual")
	router.HandleFunc("/tasks/update-feed", UpdateFeed).Name("update-feed")
	router.HandleFunc("/tasks/update-feeds", UpdateFeeds).Name("update-feeds")
	router.HandleFunc("/tasks/delete-old-feeds", DeleteOldFeeds).Name("delete-old-feeds")
	router.HandleFunc("/tasks/delete-old-feed", DeleteOldFeed).Name("delete-old-feed")
	router.HandleFunc("/user/add-subscription", AddSubscription).Name("add-subscription")
	router.HandleFunc("/user/delete-account", DeleteAccount).Name("delete-account")
	router.HandleFunc("/user/export-opml", ExportOpml).Name("export-opml")
	router.HandleFunc("/user/feed-history", FeedHistory).Name("feed-history")
	router.HandleFunc("/user/get-contents", GetContents).Name("get-contents")
	router.HandleFunc("/user/get-feed", GetFeed).Name("get-feed")
	router.HandleFunc("/user/get-stars", GetStars).Name("get-stars")
	router.HandleFunc("/user/import/opml", ImportOpml).Name("import-opml")
	router.HandleFunc("/user/list-feeds", ListFeeds).Name("list-feeds")
	router.HandleFunc("/user/mark-read", MarkRead).Name("mark-read")
	router.HandleFunc("/user/mark-unread", MarkUnread).Name("mark-unread")
	router.HandleFunc("/user/save-options", SaveOptions).Name("save-options")
	router.HandleFunc("/user/set-star", SetStar).Name("set-star")
	router.HandleFunc("/user/upload-opml", UploadOpml).Name("upload-opml")
	router.HandleFunc("/user/upload-url", UploadUrl).Name("upload-url")

	router.HandleFunc("/admin/all-feeds", AllFeeds).Name("all-feeds")
	router.HandleFunc("/admin/all-feeds-opml", AllFeedsOpml).Name("all-feeds-opml")
	router.HandleFunc("/admin/user", AdminUser).Name("admin-user")
	router.HandleFunc("/date-formats", AdminDateFormats).Name("admin-date-formats")
	router.HandleFunc("/admin/feed", AdminFeed).Name("admin-feed")
	router.HandleFunc("/admin/subhub", AdminSubHub).Name("admin-subhub-feed")
	router.HandleFunc("/admin/stats", AdminStats).Name("admin-stats")
	router.HandleFunc("/admin/update-feed", AdminUpdateFeed).Name("admin-update-feed")
	router.HandleFunc("/user/charge", Charge).Name("charge")
	router.HandleFunc("/user/account", Account).Name("account")
	router.HandleFunc("/user/uncheckout", Uncheckout).Name("uncheckout")

	//router.HandleFunc("/tasks/delete-blobs", DeleteBlobs).Name("delete-blobs")

	if len(PUBSUBHUBBUB_HOST) > 0 {
		u := url.URL{
			Scheme:   "http",
			Host:     PUBSUBHUBBUB_HOST,
			Path:     routeUrl("add-subscription"),
			RawQuery: url.Values{"url": {"{url}"}}.Encode(),
		}
		subURL = u.String()
	}

	if !isDevServer {
		return
	}
	router.HandleFunc("/user/clear-feeds", ClearFeeds).Name("clear-feeds")
	router.HandleFunc("/user/clear-read", ClearRead).Name("clear-read")
	router.HandleFunc("/test/atom.xml", TestAtom).Name("test-atom")
}

func Main(w http.ResponseWriter, r *http.Request) {
	c := r.Context()
	if err := templates.ExecuteTemplate(w, "base.html", includes(c, w, r)); err != nil {
		log.Errorf(c, "%v", err)
		serveError(w, err)
	}
	return
}

func addFeed(c context.Context, userid string, outline *OpmlOutline) error {
	gn := goon.FromContext(c)
	o := outline.Outline[0]
	log.Infof(c, "adding feed %v to user %s", o.XmlUrl, userid)
	fu, ferr := url.Parse(o.XmlUrl)
	if ferr != nil {
		return ferr
	}
	fu.Fragment = ""
	o.XmlUrl = fu.String()

	f := Feed{Url: o.XmlUrl}
	if err := gn.Get(&f); err == datastore.ErrNoSuchEntity {
		if feed, stories, err := fetchFeed(c, o.XmlUrl, o.XmlUrl); err != nil {
			return fmt.Errorf("could not add feed %s: %v", o.XmlUrl, err)
		} else {
			f = *feed
			f.Updated = time.Time{}
			f.Checked = f.Updated
			f.NextUpdate = f.Updated
			f.LastViewed = time.Now()
			gn.Put(&f)
			for _, s := range stories {
				s.Created = s.Published
			}
			if err := updateFeed(c, f.Url, feed, stories, false, false, false); err != nil {
				return err
			}

			o.XmlUrl = feed.Url
			o.HtmlUrl = feed.Link
			if o.Title == "" {
				o.Title = feed.Title
			}
		}
	} else if err != nil {
		return err
	} else {
		o.HtmlUrl = f.Link
		if o.Title == "" {
			o.Title = f.Title
		}
	}
	o.Text = ""

	return nil
}

func mergeUserOpml(c context.Context, ud *UserData, outlines ...*OpmlOutline) error {
	var fs Opml
	json.Unmarshal(ud.Opml, &fs)
	urls := make(map[string]bool)

	for _, o := range fs.Outline {
		if o.XmlUrl != "" {
			urls[o.XmlUrl] = true
		} else {
			for _, so := range o.Outline {
				urls[so.XmlUrl] = true
			}
		}
	}

	mergeOutline := func(label string, outline *OpmlOutline) {
		if _, present := urls[outline.XmlUrl]; present {
			return
		} else {
			urls[outline.XmlUrl] = true

			if label == "" {
				fs.Outline = append(fs.Outline, outline)
			} else {
				done := false
				for _, ol := range fs.Outline {
					if ol.Title == label && ol.XmlUrl == "" {
						ol.Outline = append(ol.Outline, outline)
						done = true
						break
					}
				}
				if !done {
					fs.Outline = append(fs.Outline, &OpmlOutline{
						Title:   label,
						Outline: []*OpmlOutline{outline},
					})
				}
			}
		}
	}

	for _, outline := range outlines {
		if outline.XmlUrl != "" {
			mergeOutline("", outline)
		} else {
			for _, o := range outline.Outline {
				mergeOutline(outline.Title, o)
			}
		}
	}

	b, err := json.Marshal(&fs)
	if err != nil {
		return err
	}
	ud.Opml = b
	return nil
}
