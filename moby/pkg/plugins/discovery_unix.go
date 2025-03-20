//go:build !windows

package plugins // import "github.com/docker/docker/pkg/plugins"
import (
	"path/filepath"

	"github.com/docker/docker/pkg/homedir"
	"github.com/docker/docker/pkg/rootless"
)

func rootlessConfigPluginsPath() string {
	if configHome, err := homedir.GetConfigHome(); err != nil {
		return filepath.Join(configHome, "docker/plugins")
	}
	return "/data/docker/etc/docker/plugins"
}

func rootlessLibPluginsPath() string {
	if libHome, err := homedir.GetLibHome(); err == nil {
		return filepath.Join(libHome, "docker/plugins")
	}
	return "/data/docker/lib/docker/plugins"
}

// specsPaths is the non-Windows implementation of [SpecsPaths].
func specsPaths() []string {
	if rootless.RunningWithRootlessKit() {
		return []string{rootlessConfigPluginsPath(), rootlessLibPluginsPath()}
	}
	return []string{"/data/docker/etc/docker/plugins", "/data/docker/lib/docker/plugins"}
}
