# go read

a google reader clone built with go on app engine and angularjs

The [original goread project](https://github.com/mjibson/goread/issues)
has been archived. This is a fork, updated for compatibility with go111
by [Michael Blakeley](https://github.com/mblakele).

## setting up a local dev environment
1. Install [Git](http://gitscm.com/) and make sure `git` is in your `PATH`.
1. Install the [Go App Engine SDK](https://cloud.google.com/appengine/docs/standard/go/setting-up-environment).
1. Set your `GOPATH` (to something like `/home/user`), and make sure it's a directory that exists. (Note: set this on your machine's environment, not in the go.bat file.)
1. Download goread and dependencies by running: `go get -d github.com/msde/goread`. You may get messages about unrecognized imports. Ignore them.
1. `cd $GOPATH/src/github.com/msde/goread/app`.
1. Copy `app.sample.yaml` to `app.yaml`.
1. `cd ..`
1. Copy `settings.go.dist` to `settings.go`.

### running locally

1. `(cd app && dev_appserver.py app.yaml)` (On Windows, you may need `python C:\go_appengine\dev_appserver.py app.yaml`)
1. View at [localhost:8080](http://localhost:8080), admin console at [localhost:8000](http://localhost:8000).
 
### resetting the local environment

1. Press `c` to clear all feeds and stories, remove all your subscriptions, and reset your unread date.

## self host on production app engine servers

1. Set up a local dev environment as described above.
1. Create a [new app engine application](https://cloud.google.com/appengine/docs/standard/go/quickstart).
1. Set the application as default : `gcloud config set project [PROJECT ID]`
1. Deploy: `(cd app && gcloud app deploy && gcloud app deploy cron.yaml)`

### other useful commands

```
(cd app && gcloud app deploy --no-promote)
(cd app && gcloud app deploy)
(cd app && gcloud app deploy cron.yaml)
gcloud app logs tail -s default
gcloud app browse
```
