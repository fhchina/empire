package heroku

import (
	"net/http"
	"strconv"

	"github.com/bgentry/heroku-go"
	"github.com/gorilla/mux"
	"github.com/remind101/empire/empire"
	"golang.org/x/net/context"
)

type Release heroku.Release

// newRelease decorates an empire.Release as a heroku.Release.
func newRelease(r *empire.Release) *Release {
	return &Release{
		Id:      string(r.ID),
		Version: int(r.Ver),
		Slug: &struct {
			Id string `json:"id"`
		}{
			Id: string(r.SlugID),
		},
		Description: r.Description,
		CreatedAt:   r.CreatedAt,
	}
}

type GetRelease struct {
	*empire.Empire
}

func (h *GetRelease) ServeHTTPContext(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	a, err := findApp(r, h)
	if err != nil {
		return err
	}

	vars := mux.Vars(r)
	vers, err := strconv.Atoi(vars["version"])
	if err != nil {
		return err
	}

	rel, err := h.ReleasesFindByAppAndVersion(a, vers)
	if err != nil {
		return err
	}

	w.WriteHeader(200)
	return Encode(w, newRelease(rel))
}

type GetReleases struct {
	*empire.Empire
}

func (h *GetReleases) ServeHTTPContext(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	a, err := findApp(r, h)
	if err != nil {
		return err
	}

	rels, err := h.ReleasesFindByApp(a)
	if err != nil {
		return err
	}

	w.WriteHeader(200)
	return Encode(w, rels)
}

type PostReleases struct {
	*empire.Empire
}

type PostReleasesForm struct {
	Version string `json:"release"`
}

func (p *PostReleasesForm) ReleaseVersion() (int, error) {
	vers, err := strconv.Atoi(p.Version)
	if err != nil {
		return vers, err
	}

	return vers, nil
}

func (h *PostReleases) ServeHTTPContext(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	var form PostReleasesForm

	if err := Decode(r, &form); err != nil {
		return err
	}

	version, err := form.ReleaseVersion()
	if err != nil {
		return err
	}

	app, err := findApp(r, h)
	if err != nil {
		return err
	}

	release, err := h.ReleasesRollback(app, version)
	if err != nil {
		return err
	}

	w.WriteHeader(200)
	return Encode(w, release)
}
