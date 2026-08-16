package mora

import (
	"context"
	"errors"
	"github.com/creativeprojects/go-selfupdate"
	"io"
	"time"
)

type fakeAppSource struct {
	releases []selfupdate.SourceRelease
	err      error
}

func (s *fakeAppSource) ListReleases(context.Context, selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
	return s.releases, s.err
}

func (*fakeAppSource) DownloadReleaseAsset(context.Context, *selfupdate.Release, int64) (io.ReadCloser, error) {
	return nil, errors.New("unexpected download")
}

type fakeAppRelease struct {
	tag        string
	draft      bool
	prerelease bool
	assets     []selfupdate.SourceAsset
}

func (r fakeAppRelease) GetID() int64              { return 1 }
func (r fakeAppRelease) GetTagName() string        { return r.tag }
func (r fakeAppRelease) GetDraft() bool            { return r.draft }
func (r fakeAppRelease) GetPrerelease() bool       { return r.prerelease }
func (r fakeAppRelease) GetPublishedAt() time.Time { return time.Now() }
func (r fakeAppRelease) GetReleaseNotes() string   { return "" }
func (r fakeAppRelease) GetName() string           { return r.tag }
func (r fakeAppRelease) GetURL() string {
	return "https://github.com/pyranthus-hq/mora/releases/tag/" + r.tag
}
func (r fakeAppRelease) GetAssets() []selfupdate.SourceAsset { return r.assets }

type fakeAppAsset struct {
	id   int64
	name string
	size int
	url  string
}

func (a fakeAppAsset) GetID() int64                  { return a.id }
func (a fakeAppAsset) GetName() string               { return a.name }
func (a fakeAppAsset) GetSize() int                  { return a.size }
func (a fakeAppAsset) GetBrowserDownloadURL() string { return a.url }
