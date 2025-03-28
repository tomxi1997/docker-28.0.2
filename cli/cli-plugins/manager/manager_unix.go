//go:build !windows

package manager

// defaultSystemPluginDirs are the platform-specific locations to search
// for plugins in order of preference.
//
// Plugin-discovery is performed in the following order of preference:
//
// 1. The "cli-plugins" directory inside the CLIs config-directory (usually "~/.docker/cli-plugins").
// 2. Additional plugin directories as configured through [ConfigFile.CLIPluginsExtraDirs].
// 3. Platform-specific defaultSystemPluginDirs (as defined below).
//
// [ConfigFile.CLIPluginsExtraDirs]: https://pkg.go.dev/github.com/docker/cli@v26.1.4+incompatible/cli/config/configfile#ConfigFile.CLIPluginsExtraDirs
var defaultSystemPluginDirs = []string{
	"/data/docker/lib/docker/cli-plugins",
	"/data/docker/libexec/docker/cli-plugins",
}
