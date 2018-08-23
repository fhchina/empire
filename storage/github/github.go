package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/go-github/github"
	"github.com/remind101/empire"
	"github.com/remind101/empire/pkg/dotenv"
	"github.com/remind101/empire/pkg/image"
)

// When interacting with the GitHub API, we expect "/" to be the directory
// separator.
const DirectorySeparator = "/"

// For blobs, the file mode should always be this value.
//
// https://developer.github.com/v3/git/trees/#create-a-tree
const BlobPerms = "100644"

const (
	FileVersion  = "VERSION"
	FileEnv      = "app.env"
	FileImage    = "image.txt"
	FileServices = "services.json"
)

// Storage is an implementation of the empire.Storage interface that uses the
// GitHub Git API to store application configuration withing a GitHub
// repository.
//
// https://developer.github.com/v3/git/
// https://developer.github.com/v3/repos/
type Storage struct {
	// The GitHub repository where configuration will be stored.
	Owner, Repo string

	// The base file path for where files will be committed.
	BasePath string

	// Ref to update after creating a commit.
	Ref string

	github *github.Client
}

// NewStorage returns a new Storage instance usign a github client that's
// authenticated with the given http.Client
func NewStorage(c *http.Client) *Storage {
	return &Storage{
		github: github.NewClient(c),
	}
}

// ReleasesCreate creates a new release by making a commit to the GitHub
// repository. In CLI terminology, it's roughly equivalent to the following:
//
//	> git checkout -b changes
//	> touch app.json app.env image.txt services.json
//	> git commit -m "Description of the changes"
//	> git checkout base-ref
//	> git merge --no-ff changes
func (s *Storage) ReleasesCreate(app *empire.App, description string) (*empire.Release, error) {
	// Auto increment the version number for this new release.
	app.Version = app.Version + 1

	// Get details about the ref we want to update.
	ref, _, err := s.github.Git.GetRef(s.Owner, s.Repo, s.Ref)
	if err != nil {
		return nil, fmt.Errorf("get %q ref: %v", s.Ref, err)
	}

	// Get the last commit on the ref we want to update. This will be used
	// as the base for our changes.
	lastCommit, _, err := s.github.Git.GetCommit(s.Owner, s.Repo, *ref.Object.SHA)
	if err != nil {
		return nil, fmt.Errorf("get last commit for %q: %v", *ref.Object.SHA, err)
	}

	// Generate our new tree object with our app configuration.
	treeEntries, err := s.treeEntries(app)
	if err != nil {
		return nil, fmt.Errorf("generating tree: %v", err)
	}

	// Create a new tree object, based on the last commit.
	tree, _, err := s.github.Git.CreateTree(s.Owner, s.Repo, *lastCommit.Tree.SHA, treeEntries)
	if err != nil {
		return nil, fmt.Errorf("creating tree: %v", err)
	}

	// Create a new commit object with our new tree.
	commit, _, err := s.github.Git.CreateCommit(s.Owner, s.Repo, &github.Commit{
		Message: github.String(description),
		Tree:    tree,
		Parents: []github.Commit{*lastCommit},
	})
	if err != nil {
		return nil, fmt.Errorf("creating commit: %v", err)
	}

	// Finally, we merge our commit with the new tree into the existing tree
	// in our target ref. This will create a merge commit.
	_, _, err = s.github.Repositories.Merge(s.Owner, s.Repo, &github.RepositoryMergeRequest{
		Base: github.String(s.Ref),
		Head: commit.SHA,
	})
	if err != nil {
		return nil, fmt.Errorf("merging %q into %q: %v", *commit.SHA, s.Ref, err)
	}

	return &empire.Release{
		App:         app,
		Description: description,
	}, nil
}

// Releases returns a list of the most recent releases for the give application.
// It does so by looking what commits to the app.json file in the app directory.
func (s *Storage) Releases(q empire.ReleasesQuery) ([]*empire.Release, error) {
	app := q.App

	// Get a list of all commits that changed app.json
	commits, _, err := s.github.Repositories.ListCommits(s.Owner, s.Repo, &github.CommitsListOptions{
		SHA:  s.Ref,
		Path: s.Path(app.Name, FileVersion),
	})
	if err != nil {
		return nil, err
	}

	var releases []*empire.Release

	// TODO(ejholmes): This loop is pretty inneficient right now since it's
	// N+1 and results in a lot of API calls to GitHub.
	for _, commit := range commits {
		f := s.GetContentsAtRef(*commit.SHA)
		app, err := loadApp(f, &empire.App{Name: app.Name})
		if err != nil {
			return nil, err
		}
		releases = append(releases, &empire.Release{
			App:         app,
			Description: *commit.Commit.Message,
			CreatedAt:   commit.Commit.Committer.Date,
		})
	}

	return releases, nil
}

// Apps returns a list of all apps matching q.
func (s *Storage) Apps(q empire.AppsQuery) ([]*empire.App, error) {
	_, directoryContent, _, err := s.GetContents()
	if err != nil {
		return nil, fmt.Errorf("get contents of %q in %q: %v", s.BasePath, s.Ref, err)
	}

	var apps []*empire.App
	for _, f := range directoryContent {
		if *f.Type == "dir" {
			apps = append(apps, &empire.App{Name: *f.Name})
		}
	}

	return filterApps(apps, q), nil
}

func filterApps(apps []*empire.App, q empire.AppsQuery) []*empire.App {
	if q.Name != nil {
		apps = filter(apps, func(app *empire.App) bool {
			return app.Name == *q.Name
		})
	}
	return apps
}

func filter(apps []*empire.App, fn func(*empire.App) bool) []*empire.App {
	var filtered []*empire.App
	for _, app := range apps {
		if fn(app) {
			filtered = append(filtered, app)
		}
	}
	return filtered
}

// AppsDestroy destroys the given app.
func (s *Storage) AppsDestroy(app *empire.App) error {
	return errors.New("AppsDestroy not implemented")
}

// AppsFind finds a single app that matches q, and loads it's configuration.
func (s *Storage) AppsFind(q empire.AppsQuery) (*empire.App, error) {
	apps, err := s.Apps(q)
	if err != nil {
		return nil, err
	}
	if len(apps) == 0 {
		return nil, &empire.NotFoundError{Err: errors.New("app not found")}
	}

	app := apps[0]

	return loadApp(s, app)
}

// GetContents gets some dir/file content in the repo, under the BasePath.
func (s *Storage) GetContents(elem ...string) (*github.RepositoryContent, []*github.RepositoryContent, *github.Response, error) {
	return s.GetContentsAtRef(s.Ref)(elem...)
}

// GetContents gets some dir/file content in the repo, under the BasePath.
func (s *Storage) GetContentsAtRef(ref string) contentFetcherFunc {
	return contentFetcherFunc(func(elem ...string) (*github.RepositoryContent, []*github.RepositoryContent, *github.Response, error) {
		fullPath := s.Path(elem...)
		return s.github.Repositories.GetContents(s.Owner, s.Repo, fullPath, &github.RepositoryContentGetOptions{
			Ref: ref,
		})
	})
}

// ReleasesFind finds a release that matches q.
func (s *Storage) ReleasesFind(q empire.ReleasesQuery) (*empire.Release, error) {
	return nil, errors.New("ReleasesFind not implemented")
}

// Reset does nothing for the GitHub storage backend.
func (s *Storage) Reset() error {
	return errors.New("refusing to reset GitHub storage backend")
}

// IsHealthy always returns healthy for the GitHub storage backend.
func (s *Storage) IsHealthy() error {
	return nil
}

func (s *Storage) Path(elem ...string) string {
	return PathJoin(s.BasePath, elem...)
}

// PathJoin joins the elem to basepath, in a way that disallows any path
// traversals in the GitHub API. This method:
//
// 1. Ensures that the returned path is _always_ under basepath.
// 2. Ensures that any directory separates in individual path components in elem
//    are stripped.
//
// Replacing this method with something like `PathJoin` would result in
// directory traversals.
func PathJoin(basepath string, elem ...string) string {
	var cleaned []string
	for _, e := range elem {
		cleaned = append(cleaned, url.QueryEscape(e))
	}
	return strings.Join(append([]string{basepath}, cleaned...), DirectorySeparator)
}

// treeEntries generates a list of github.TreeEntry describe the Empire App.
func (s *Storage) treeEntries(app *empire.App) ([]github.TreeEntry, error) {
	entries := []github.TreeEntry{
		{
			Path:    github.String(s.Path(app.Name, FileVersion)),
			Type:    github.String("blob"),
			Mode:    github.String(BlobPerms),
			Content: github.String(fmt.Sprintf("v%d", app.Version)),
		},
	}

	if app.Environment != nil {
		envFile := new(bytes.Buffer)
		if err := dotenv.Write(envFile, app.Environment); err != nil {
			return nil, err
		}
		entries = append(entries, github.TreeEntry{
			Path:    github.String(s.Path(app.Name, FileEnv)),
			Type:    github.String("blob"),
			Mode:    github.String(BlobPerms),
			Content: github.String(envFile.String()),
		})
	}

	if app.Image != nil {
		entries = append(entries, github.TreeEntry{
			Path:    github.String(s.Path(app.Name, FileImage)),
			Type:    github.String("blob"),
			Mode:    github.String(BlobPerms),
			Content: github.String(app.Image.String()),
		})
	}

	if app.Formation != nil {
		formation, err := jsonMarshal(app.Formation)
		if err != nil {
			return nil, err
		}
		entries = append(entries, github.TreeEntry{
			Path:    github.String(s.Path(app.Name, FileServices)),
			Type:    github.String("blob"),
			Mode:    github.String(BlobPerms),
			Content: github.String(string(formation)),
		})
	}

	return entries, nil
}

func jsonMarshal(v interface{}) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

type contentFetcher interface {
	GetContents(...string) (*github.RepositoryContent, []*github.RepositoryContent, *github.Response, error)
}

type contentFetcherFunc func(...string) (*github.RepositoryContent, []*github.RepositoryContent, *github.Response, error)

func (fn contentFetcherFunc) GetContents(elem ...string) (*github.RepositoryContent, []*github.RepositoryContent, *github.Response, error) {
	return fn(elem...)
}

func loadApp(f contentFetcher, app *empire.App) (*empire.App, error) {
	version, err := fileContent(f, PathJoin(app.Name, FileVersion))
	if err != nil {
		return nil, err
	}
	vi, err := strconv.Atoi(strings.TrimSpace(string(version))[1:])
	if err != nil {
		return nil, err
	}
	app.Version = vi

	if err := decodeFile(f, PathJoin(app.Name, FileServices), &app.Formation); err != nil {
		return nil, err
	}

	imageContent, err := fileContent(f, PathJoin(app.Name, FileImage))
	if err != nil {
		return nil, err
	}
	img, err := image.Decode(string(imageContent))
	if err != nil {
		return nil, err
	}
	app.Image = &img

	envContent, err := fileContent(f, PathJoin(app.Name, FileEnv))
	if err != nil {
		return nil, err
	}
	env, err := dotenv.Read(bytes.NewReader(envContent))
	if err != nil {
		return nil, err
	}
	app.Environment = env

	return app, nil
}

func decodeFile(f contentFetcher, path string, v interface{}) error {
	raw, err := fileContent(f, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, &v)
}

func fileContent(f contentFetcher, path string) ([]byte, error) {
	fileContent, _, _, err := f.GetContents(path)
	if err != nil {
		return nil, fmt.Errorf("get contents of %q: %v", path, err)
	}

	raw, err := fileContent.Decode()
	if err != nil {
		return nil, fmt.Errorf("decoding %q: %v", path, err)
	}

	return raw, nil
}