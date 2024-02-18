package image

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	"github.com/buildpacks/imgutil/layout"
	"github.com/buildpacks/imgutil/layout/sparse"

	"github.com/buildpacks/imgutil"
	"github.com/buildpacks/imgutil/local"
	"github.com/buildpacks/imgutil/remote"
	"github.com/buildpacks/lifecycle/auth"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/pkg/errors"

	pname "github.com/buildpacks/pack/internal/name"
	"github.com/buildpacks/pack/internal/style"
	"github.com/buildpacks/pack/internal/term"
	"github.com/buildpacks/pack/pkg/logging"
)

// FetcherOption is a type of function that mutate settings on the client.
// Values in these functions are set through currying.
type FetcherOption func(c *Fetcher)

type LayoutOption struct {
	Path   string
	Sparse bool
}

// WithRegistryMirrors supply your own mirrors for registry.
func WithRegistryMirrors(registryMirrors map[string]string) FetcherOption {
	return func(c *Fetcher) {
		c.registryMirrors = registryMirrors
	}
}

func WithKeychain(keychain authn.Keychain) FetcherOption {
	return func(c *Fetcher) {
		c.keychain = keychain
	}
}

type DockerClient interface {
	local.DockerClient
	ImagePull(ctx context.Context, ref string, options types.ImagePullOptions) (io.ReadCloser, error)
}

type Fetcher struct {
	docker          DockerClient
	logger          logging.Logger
	registryMirrors map[string]string
	keychain        authn.Keychain
}

type FetchOptions struct {
	Daemon       bool
	Platform     string
	PullPolicy   PullPolicy
	LayoutOption LayoutOption
}

func NewFetcher(logger logging.Logger, docker DockerClient, opts ...FetcherOption) *Fetcher {
	fetcher := &Fetcher{
		logger:   logger,
		docker:   docker,
		keychain: authn.DefaultKeychain,
	}

	for _, opt := range opts {
		opt(fetcher)
	}

	return fetcher
}

var ErrNotFound = errors.New("not found")

func (f *Fetcher) Fetch(ctx context.Context, name string, options FetchOptions) (imgutil.Image, error) {
	name, err := pname.TranslateRegistry(name, f.registryMirrors, f.logger)
	if err != nil {
		return nil, err
	}

	var imageID string // Variable to store the image ID
	parts := strings.Split(name, "@")
	if len(parts) > 1 {
		imageID = parts[1]
	} else {
		imageID = parts[0]
	}

	if (options.LayoutOption != LayoutOption{}) {
		return f.fetchLayoutImage(name, options.LayoutOption)
	}

	if !options.Daemon {
		return f.fetchRemoteImage(name)
	}

	switch options.PullPolicy {
	case PullNever:
		img, err := f.fetchDaemonImage(name)
		return img, err
	case PullIfNotPresent:
		img, err := f.fetchDaemonImage(name)
		if err == nil || !errors.Is(err, ErrNotFound) {
			return img, err
		}
	case PullWithInterval:
		pull, err := f.CheckImagePullInterval(imageID)
		if err != nil {
			return nil, err
		}
		img, err := f.fetchDaemonImage(name)
		if !pull && err != nil && !errors.Is(err, ErrNotFound) {
			return img, err
		}
		err = f.PruneOldImages()
		if err != nil {
			f.logger.Warnf("Failed to prune images that are older than 7 days, %s", err)
		}
	}

	f.logger.Debugf("Pulling image %s", style.Symbol(name))
	if err = f.pullImage(ctx, name, options.Platform); err != nil {
		// sample error from docker engine:
		// image with reference <image> was found but does not match the specified platform: wanted linux/amd64, actual: linux
		if strings.Contains(err.Error(), "does not match the specified platform") {
			err = f.pullImage(ctx, name, "")
		}
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	image, err := f.fetchDaemonImage(name)
	if err != nil {
		return nil, err
	}

	// Update image pull record in the JSON file
	if err := f.updateImagePullRecord(imageID, time.Now().Format(time.RFC3339)); err != nil {
		return nil, err
	}

	return image, nil
}

func (f *Fetcher) fetchDaemonImage(name string) (imgutil.Image, error) {
	image, err := local.NewImage(name, f.docker, local.FromBaseImage(name))
	if err != nil {
		return nil, err
	}

	if !image.Found() {
		return nil, errors.Wrapf(ErrNotFound, "image %s does not exist on the daemon", style.Symbol(name))
	}

	return image, nil
}

func (f *Fetcher) fetchRemoteImage(name string) (imgutil.Image, error) {
	image, err := remote.NewImage(name, f.keychain, remote.FromBaseImage(name))
	if err != nil {
		return nil, err
	}

	if !image.Found() {
		return nil, errors.Wrapf(ErrNotFound, "image %s does not exist in registry", style.Symbol(name))
	}

	return image, nil
}

func (f *Fetcher) fetchLayoutImage(name string, options LayoutOption) (imgutil.Image, error) {
	var (
		image imgutil.Image
		err   error
	)

	v1Image, err := remote.NewV1Image(name, f.keychain)
	if err != nil {
		return nil, err
	}

	if options.Sparse {
		image, err = sparse.NewImage(options.Path, v1Image)
	} else {
		image, err = layout.NewImage(options.Path, layout.FromBaseImage(v1Image))
	}

	if err != nil {
		return nil, err
	}

	err = image.Save()
	if err != nil {
		return nil, err
	}

	return image, nil
}

func (f *Fetcher) pullImage(ctx context.Context, imageID string, platform string) error {
	regAuth, err := f.registryAuth(imageID)
	if err != nil {
		return err
	}

	rc, err := f.docker.ImagePull(ctx, imageID, types.ImagePullOptions{RegistryAuth: regAuth, Platform: platform})
	if err != nil {
		if client.IsErrNotFound(err) {
			return errors.Wrapf(ErrNotFound, "image %s does not exist on the daemon", style.Symbol(imageID))
		}

		return err
	}

	writer := logging.GetWriterForLevel(f.logger, logging.InfoLevel)
	termFd, isTerm := term.IsTerminal(writer)

	err = jsonmessage.DisplayJSONMessagesStream(rc, &colorizedWriter{writer}, termFd, isTerm, nil)
	if err != nil {
		return err
	}

	return rc.Close()
}

func (f *Fetcher) registryAuth(ref string) (string, error) {
	_, a, err := auth.ReferenceForRepoName(f.keychain, ref)
	if err != nil {
		return "", errors.Wrapf(err, "resolve auth for ref %s", ref)
	}
	authConfig, err := a.Authorization()
	if err != nil {
		return "", err
	}

	dataJSON, err := json.Marshal(authConfig)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(dataJSON), nil
}

type colorizedWriter struct {
	writer io.Writer
}

type colorFunc = func(string, ...interface{}) string

func (w *colorizedWriter) Write(p []byte) (n int, err error) {
	msg := string(p)
	colorizers := map[string]colorFunc{
		"Waiting":           style.Waiting,
		"Pulling fs layer":  style.Waiting,
		"Downloading":       style.Working,
		"Download complete": style.Working,
		"Extracting":        style.Working,
		"Pull complete":     style.Complete,
		"Already exists":    style.Complete,
		"=":                 style.ProgressBar,
		">":                 style.ProgressBar,
	}
	for pattern, colorize := range colorizers {
		msg = strings.ReplaceAll(msg, pattern, colorize(pattern))
	}
	return w.writer.Write([]byte(msg))
}

func (f *Fetcher) updateImagePullRecord(imageID, timestamp string) error {
	imageJSON, err := readImageJSON(f.logger)
	if err != nil {
		return err
	}

	if imageJSON.Image.ImageIDtoTIME == nil {
		imageJSON.Image.ImageIDtoTIME = make(map[string]string)
	}
	imageJSON.Image.ImageIDtoTIME[imageID] = timestamp

	updatedJSON, err := json.MarshalIndent(imageJSON, "", "    ")
	if err != nil {
		return errors.Wrap(err, "failed to marshal updated records")
	}

	return os.WriteFile(imagePath, updatedJSON, 0644)
}
