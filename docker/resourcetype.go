package docker

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/puppetlabs/wash/datastore"
	"github.com/puppetlabs/wash/log"
	"github.com/puppetlabs/wash/plugin"
)

type resourcetype struct {
	*root
	typename string
	reqs     sync.Map
}

func newResourceTypes(cli *root) map[string]*resourcetype {
	resourcetypes := make(map[string]*resourcetype)
	// Use individual caches for slower resources like volumes to control the timeout.
	for _, name := range []string{"container", "volume"} {
		resourcetypes[name] = &resourcetype{root: cli, typename: name}
	}
	return resourcetypes
}

// Find the instance of the resource type by its ID
func (cli *resourcetype) Find(ctx context.Context, name string) (plugin.Node, error) {
	switch cli.typename {
	case "container":
		containers, err := cli.cachedContainerList(ctx)
		if err != nil {
			return nil, err
		}
		if ok := datastore.ContainsString(containers, name); ok {
			log.Debugf("Found container %v", name)
			return plugin.NewFile(&container{cli, name}), nil
		}
		log.Debugf("Container %v not found in %v", name, cli)
		return nil, plugin.ENOENT
	case "volume":
		volumes, err := cli.cachedVolumeList(ctx)
		if err != nil {
			return nil, err
		}
		if ok := datastore.ContainsString(volumes, name); ok {
			log.Debugf("Found volume %v", name)
			return plugin.NewDir(newVolume(cli, name)), nil
		}
		log.Debugf("Volume %v not found in %v", name, cli)
		return nil, plugin.ENOENT
	}
	return nil, plugin.ENOTSUP
}

// List all instances of the resource type as files.
func (cli *resourcetype) List(ctx context.Context) ([]plugin.Node, error) {
	switch cli.typename {
	case "container":
		containers, err := cli.cachedContainerList(ctx)
		if err != nil {
			return nil, err
		}
		log.Debugf("Listing %v containers in %v", len(containers), cli)
		keys := make([]plugin.Node, len(containers))
		for i, inst := range containers {
			keys[i] = plugin.NewFile(&container{cli, inst})
		}
		return keys, nil
	case "volume":
		volumes, err := cli.cachedVolumeList(ctx)
		if err != nil {
			return nil, err
		}
		log.Debugf("Listing %v volumes in %v", len(volumes), cli)
		keys := make([]plugin.Node, len(volumes))
		for i, vol := range volumes {
			keys[i] = plugin.NewDir(newVolume(cli, vol))
		}
		return keys, nil
	}
	return nil, plugin.ENOTSUP
}

// A unique string describing the resource type.
func (cli *resourcetype) String() string {
	return cli.root.Name() + "/" + cli.Name()
}

// Name returns the name of the resource type.
func (cli *resourcetype) Name() string {
	return cli.typename
}

// Attr returns attributes of the resource type.
func (cli *resourcetype) Attr(ctx context.Context) (*plugin.Attributes, error) {
	// Now that content updates are asynchronous, we can make directory mtime reflect when we get new content.
	// TODO: make this more constrained to the specific resource.
	latest := cli.updated
	cli.reqs.Range(func(k, v interface{}) bool {
		if updated := v.(*datastore.StreamBuffer).LastUpdate(); updated.After(latest) {
			latest = updated
		}
		return true
	})
	return &plugin.Attributes{Mtime: latest}, nil
}

// Xattr returns a map of extended attributes.
func (cli *resourcetype) Xattr(ctx context.Context) (map[string][]byte, error) {
	return map[string][]byte{}, nil
}

func (cli *resourcetype) cachedContainerList(ctx context.Context) ([]string, error) {
	return cli.cache.CachedStrings(cli.String(), func() ([]string, error) {
		containers, err := cli.ContainerList(ctx, types.ContainerListOptions{})
		if err != nil {
			return nil, err
		}
		strings := make([]string, len(containers))
		for i, container := range containers {
			strings[i] = container.ID
		}
		cli.updated = time.Now()
		return strings, nil
	})
}

func (cli *resourcetype) cachedVolumeList(ctx context.Context) ([]string, error) {
	return cli.cache.CachedStrings(cli.String(), func() ([]string, error) {
		volumes, err := cli.VolumeList(ctx, filters.Args{})
		if err != nil {
			return nil, err
		}
		strings := make([]string, len(volumes.Volumes))
		for i, volume := range volumes.Volumes {
			strings[i] = volume.Name
			// Also cache 'volume', as this is the same data returned by VolumeInspect.
			// Store as JSON since that's how we'll process it.
			if js, err := json.Marshal(volume); err == nil {
				cli.cache.Set(cli.String()+"/"+volume.Name, js)
			} else {
				log.Printf("Unable to marshal volume %v to JSON: %v", volume, err)
			}
		}
		cli.updated = time.Now()
		return strings, nil
	})
}