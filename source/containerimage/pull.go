package containerimage

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/containerd/remotes/docker/schema1"
	"github.com/containerd/containerd/rootfs"
	"github.com/containerd/containerd/snapshots"
	"github.com/genuinetools/img/util/auth"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/source"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/moby/buildkit/util/imageutil"
	"github.com/moby/buildkit/util/tracing"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// TODO: break apart containerd specifics like contentstore so the resolver
// code can be used with any implementation

// SourceOpt contains the options for the container image source.
type SourceOpt struct {
	SessionManager *session.Manager
	Snapshotter    snapshot.Snapshotter
	ContentStore   content.Store
	Applier        diff.Applier
	CacheAccessor  cache.Accessor
	ImageStore     images.Store // optional
}

type imageSource struct {
	SourceOpt
	g flightcontrol.Group
}

// NewSource returns a new source object.
func NewSource(opt SourceOpt) (source.Source, error) {
	is := &imageSource{
		SourceOpt: opt,
	}

	return is, nil
}

func (is *imageSource) ID() string {
	return source.DockerImageScheme
}

func (is *imageSource) getResolver(ctx context.Context) remotes.Resolver {
	r := docker.NewResolver(docker.ResolverOptions{
		Client:      tracing.DefaultClient,
		Credentials: is.getCredentialsFromSession(ctx),
	})

	if is.ImageStore == nil {
		return r
	}

	return localFallbackResolver{r, is.ImageStore}
}

func (is *imageSource) getCredentialsFromSession(ctx context.Context) func(string) (string, string, error) {
	id := session.FromContext(ctx)
	if id == "" {
		return nil
	}
	return func(host string) (string, string, error) {
		creds, err := auth.DockerAuthCredentials(host)
		if err != nil {
			return "", "", err
		}

		return creds.Username, creds.Secret, nil
	}
}

func (is *imageSource) ResolveImageConfig(ctx context.Context, ref string) (digest.Digest, []byte, error) {
	type t struct {
		dgst digest.Digest
		dt   []byte
	}
	res, err := is.g.Do(ctx, ref, func(ctx context.Context) (interface{}, error) {
		dgst, dt, err := imageutil.Config(ctx, ref, is.getResolver(ctx), is.ContentStore)
		if err != nil {
			return nil, err
		}
		return &t{dgst: dgst, dt: dt}, nil
	})
	if err != nil {
		return "", nil, err
	}
	typed := res.(*t)
	return typed.dgst, typed.dt, nil
}

func (is *imageSource) Resolve(ctx context.Context, id source.Identifier) (source.SourceInstance, error) {
	imageIdentifier, ok := id.(*source.ImageIdentifier)
	if !ok {
		return nil, errors.Errorf("invalid image identifier %v", id)
	}

	p := &puller{
		src:      imageIdentifier,
		is:       is,
		resolver: is.getResolver(ctx),
		images:   is.ImageStore,
	}
	return p, nil
}

// A remotes.Resolver which checks the local image store if the real
// resolver cannot find the image, essentially falling back to a local
// image if one is present.
//
// We do not override the Fetcher or Pusher methods:
//
// - Fetcher is called by github.com/containerd/containerd/remotes/:fetch()
//   only after it has checked for the content locally, so avoid the
//   hassle of interposing a local-fetch proxy and simply pass on the
//   request.
// - Pusher wouldn't make sense to push locally, so just forward.

type localFallbackResolver struct {
	remotes.Resolver
	is images.Store
}

func (r localFallbackResolver) Resolve(ctx context.Context, ref string) (string, ocispec.Descriptor, error) {
	n, desc, err := r.Resolver.Resolve(ctx, ref)
	if err == nil {
		return n, desc, err
	}

	img, err2 := r.is.Get(ctx, ref)
	if err2 != nil {
		return "", ocispec.Descriptor{}, err
	}
	return ref, img.Target, nil
}

type puller struct {
	is          *imageSource
	resolveOnce sync.Once
	src         *source.ImageIdentifier
	desc        ocispec.Descriptor
	ref         string
	resolveErr  error
	resolver    remotes.Resolver
	images      images.Store
}

func (p *puller) resolve(ctx context.Context) error {
	p.resolveOnce.Do(func() {
		logrus.Infof("resolving %s", p.src.Reference.String())

		dgst := p.src.Reference.Digest()
		if dgst != "" {
			info, err := p.is.ContentStore.Info(ctx, dgst)
			if err == nil {
				p.ref = p.src.Reference.String()
				ra, err := p.is.ContentStore.ReaderAt(ctx, dgst)
				if err == nil {
					mt, err := imageutil.DetectManifestMediaType(ra)
					if err == nil {
						p.desc = ocispec.Descriptor{
							Size:      info.Size,
							Digest:    dgst,
							MediaType: mt,
						}
						return
					}
				}
			}
		}

		ref, desc, err := p.resolver.Resolve(ctx, p.src.Reference.String())
		if err != nil {
			p.resolveErr = err
			return
		}
		p.desc = desc
		p.ref = ref

		// Add to the image store.
		if p.images == nil {
			p.resolveErr = errors.New("image store is nil")
		}

		// Update the target image. Create it if it does not exist.
		img := images.Image{
			Name:      p.ref,
			Target:    p.desc,
			CreatedAt: time.Now(),
		}
		ctx = namespaces.WithNamespace(ctx, namespaces.Default)
		if _, err := p.images.Update(ctx, img); err != nil {
			if !errdefs.IsNotFound(err) {
				p.resolveErr = fmt.Errorf("updating image store for image %s failed: %v", p.ref, err)
			}

			logrus.Debugf("Creating new tag: %s", p.ref)
			if _, err := p.images.Create(ctx, img); err != nil {
				p.resolveErr = fmt.Errorf("creating image %s in image store failed: %v", p.ref, err)
			}
		}
	})
	return p.resolveErr
}

func (p *puller) CacheKey(ctx context.Context) (string, error) {
	if err := p.resolve(ctx); err != nil {
		return "", err
	}
	return p.desc.Digest.String(), nil
}

func (p *puller) Snapshot(ctx context.Context) (cache.ImmutableRef, error) {
	if err := p.resolve(ctx); err != nil {
		return nil, err
	}

	ongoing := newJobs(p.ref)

	fetcher, err := p.resolver.Fetcher(ctx, p.ref)
	if err != nil {
		return nil, err
	}

	// TODO: need a wrapper snapshot interface that combines content
	// and snapshots as 1) buildkit shouldn't have a dependency on contentstore
	// or 2) cachemanager should manage the contentstore
	handlers := []images.Handler{
		images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
			ongoing.add(desc)
			return nil, nil
		}),
	}
	var schema1Converter *schema1.Converter
	if p.desc.MediaType == images.MediaTypeDockerSchema1Manifest {
		schema1Converter = schema1.NewConverter(p.is.ContentStore, fetcher)
		handlers = append(handlers, schema1Converter)
	} else {
		// Get all the children for a descriptor
		childrenHandler := images.ChildrenHandler(p.is.ContentStore)
		// Set any children labels for that content
		childrenHandler = images.SetChildrenLabels(p.is.ContentStore, childrenHandler)
		// Filter the childen by the platform
		childrenHandler = images.FilterPlatforms(childrenHandler, platforms.Default())

		handlers = append(handlers,
			remotes.FetchHandler(p.is.ContentStore, fetcher),
			childrenHandler,
		)
	}

	if err := images.Dispatch(ctx, images.Handlers(handlers...), p.desc); err != nil {
		return nil, err
	}

	var usedBlobs, unusedBlobs []ocispec.Descriptor

	if schema1Converter != nil {
		ongoing.remove(p.desc) // Not left in the content store so this is sufficient.
		p.desc, err = schema1Converter.Convert(ctx)
		if err != nil {
			return nil, err
		}
		ongoing.add(p.desc)

		var mu sync.Mutex // images.Dispatch calls handlers in parallel
		allBlobs := make(map[digest.Digest]ocispec.Descriptor)
		for _, j := range ongoing.added {
			allBlobs[j.Digest] = j.Descriptor
		}

		handlers := []images.Handler{
			images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
				mu.Lock()
				defer mu.Unlock()
				usedBlobs = append(usedBlobs, desc)
				delete(allBlobs, desc.Digest)
				return nil, nil
			}),
			images.FilterPlatforms(images.ChildrenHandler(p.is.ContentStore), platforms.Default()),
		}

		if err := images.Dispatch(ctx, images.Handlers(handlers...), p.desc); err != nil {
			return nil, err
		}

		for _, j := range allBlobs {
			unusedBlobs = append(unusedBlobs, j)
		}
	} else {
		for _, j := range ongoing.added {
			usedBlobs = append(usedBlobs, j.Descriptor)
		}
	}

	// split all pulled data to layers and rest. layers remain roots and are deleted with snapshots. rest will be linked to layers.
	var notLayerBlobs []ocispec.Descriptor
	var layerBlobs []ocispec.Descriptor
	for _, j := range usedBlobs {
		switch j.MediaType {
		case ocispec.MediaTypeImageLayer, images.MediaTypeDockerSchema2Layer, ocispec.MediaTypeImageLayerGzip, images.MediaTypeDockerSchema2LayerGzip:
			layerBlobs = append(layerBlobs, j)
		default:
			notLayerBlobs = append(notLayerBlobs, j)
		}
	}

	for _, l := range layerBlobs {
		labels := map[string]string{}
		var fields []string
		for _, nl := range notLayerBlobs {
			k := "containerd.io/gc.ref.content." + nl.Digest.Hex()[:12]
			labels[k] = nl.Digest.String()
			fields = append(fields, "labels."+k)
		}
		if _, err := p.is.ContentStore.Update(ctx, content.Info{
			Digest: l.Digest,
			Labels: labels,
		}, fields...); err != nil {
			return nil, err
		}
	}

	for _, nl := range append(notLayerBlobs, unusedBlobs...) {
		if err := p.is.ContentStore.Delete(ctx, nl.Digest); err != nil {
			return nil, err
		}
	}

	logrus.Infof("unpacking %s", p.src.Reference.String())
	csh, release := snapshot.NewContainerdSnapshotter(p.is.Snapshotter)
	defer release()

	chainid, err := p.is.unpack(ctx, p.desc, csh)
	if err != nil {
		return nil, err
	}

	return p.is.CacheAccessor.GetFromSnapshotter(ctx, chainid, cache.WithDescription(fmt.Sprintf("pulled from %s", p.ref)))
}

func (is *imageSource) unpack(ctx context.Context, desc ocispec.Descriptor, s snapshots.Snapshotter) (string, error) {
	layers, err := getLayers(ctx, is.ContentStore, desc)
	if err != nil {
		return "", err
	}

	var chain []digest.Digest
	for _, layer := range layers {
		labels := map[string]string{
			"containerd.io/gc.root":      time.Now().UTC().Format(time.RFC3339Nano),
			"containerd.io/uncompressed": layer.Diff.Digest.String(),
		}
		if _, err := rootfs.ApplyLayer(ctx, layer, chain, s, is.Applier, snapshots.WithLabels(labels)); err != nil {
			return "", err
		}
		chain = append(chain, layer.Diff.Digest)
	}
	chainID := identity.ChainID(chain)
	if err != nil {
		return "", err
	}

	if err := is.fillBlobMapping(ctx, layers); err != nil {
		return "", err
	}

	return string(chainID), nil
}

func (is *imageSource) fillBlobMapping(ctx context.Context, layers []rootfs.Layer) error {
	var chain []digest.Digest
	for _, l := range layers {
		chain = append(chain, l.Diff.Digest)
		chainID := identity.ChainID(chain)
		if err := is.SourceOpt.Snapshotter.SetBlob(ctx, string(chainID), l.Diff.Digest, l.Blob.Digest); err != nil {
			return err
		}
	}
	return nil
}

func getLayers(ctx context.Context, provider content.Provider, desc ocispec.Descriptor) ([]rootfs.Layer, error) {
	manifest, err := images.Manifest(ctx, provider, desc, platforms.Default())
	if err != nil {
		return nil, errors.WithStack(err)
	}
	image := images.Image{Target: desc}
	diffIDs, err := image.RootFS(ctx, provider, platforms.Default())
	if err != nil {
		return nil, errors.Wrap(err, "failed to resolve rootfs")
	}
	if len(diffIDs) != len(manifest.Layers) {
		return nil, errors.Errorf("mismatched image rootfs and manifest layers %+v %+v", diffIDs, manifest.Layers)
	}
	layers := make([]rootfs.Layer, len(diffIDs))
	for i := range diffIDs {
		layers[i].Diff = ocispec.Descriptor{
			// TODO: derive media type from compressed type
			MediaType: ocispec.MediaTypeImageLayer,
			Digest:    diffIDs[i],
		}
		layers[i].Blob = manifest.Layers[i]
	}
	return layers, nil
}

// jobs provides a way of identifying the download keys for a particular task
// encountering during the pull walk.
//
// This is very minimal and will probably be replaced with something more
// featured.
type jobs struct {
	name     string
	added    map[digest.Digest]job
	mu       sync.Mutex
	resolved bool
}

type job struct {
	ocispec.Descriptor
	done    bool
	started time.Time
}

func newJobs(name string) *jobs {
	return &jobs{
		name:  name,
		added: make(map[digest.Digest]job),
	}
}

func (j *jobs) add(desc ocispec.Descriptor) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if _, ok := j.added[desc.Digest]; ok {
		return
	}
	j.added[desc.Digest] = job{
		Descriptor: desc,
		started:    time.Now(),
	}
}

func (j *jobs) remove(desc ocispec.Descriptor) {
	j.mu.Lock()
	defer j.mu.Unlock()

	delete(j.added, desc.Digest)
}

func (j *jobs) jobs() []job {
	j.mu.Lock()
	defer j.mu.Unlock()

	descs := make([]job, 0, len(j.added))
	for _, j := range j.added {
		descs = append(descs, j)
	}
	return descs
}

func (j *jobs) isResolved() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.resolved
}
