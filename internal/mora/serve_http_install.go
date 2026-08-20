package mora

import (
	"fmt"
	httpservicepkg "github.com/pyranthus-hq/mora/internal/httpservice"
	"io"
	"os"
)

var serveHTTPPortFree = func(port int) bool { return httpservicepkg.PortFree(port) }

func serveHTTPServicePort() int { return envPortOr(defaultHTTPPort) }
func serveHTTPEnvironment() map[string]string {
	out := map[string]string{}
	for _, key := range []string{"MORA_CONFIG_DIR", "MORA_VAULT", "MORA_PORT"} {
		if v := os.Getenv(key); v != "" {
			out[key] = v
		}
	}
	return out
}
func serveHTTPEnvVars() (keys, vals []string) { return httpservicepkg.EnvVars(serveHTTPEnvironment()) }

func serveHTTPOptions(cfg Config, stdout io.Writer, needExe bool, homeStrict bool) (httpservicepkg.Options, error) {
	goos := runtimeGOOS()
	exe := ""
	var err error
	if needExe {
		exe, err = os.Executable()
		if err != nil {
			return httpservicepkg.Options{}, err
		}
	}
	home := ""
	if goos != "windows" {
		home, err = os.UserHomeDir()
		if err != nil && homeStrict {
			return httpservicepkg.Options{}, err
		}
	}
	return httpservicepkg.Options{StateDir: cfg.StateDir, GOOS: goos, Home: home, Executable: exe, User: os.Getenv("USER"), UID: os.Getuid(), Port: serveHTTPServicePort(), Env: serveHTTPEnvironment(), Stdout: stdout, RunCommand: runScheduleCommand, PortFree: serveHTTPPortFree, Healthy: httpservicepkg.Healthy, OK: okf}, nil
}
func serveHTTPService(cfg Config, sub string, stdout io.Writer) error {
	switch sub {
	case "install":
		return installServeHTTP(cfg, stdout)
	case "uninstall":
		return uninstallServeHTTP(cfg, stdout)
	case "status":
		return statusServeHTTP(cfg, stdout)
	default:
		return fmt.Errorf("usage: mora serve http install|uninstall|status")
	}
}
func installServeHTTP(cfg Config, stdout io.Writer) error {
	o, err := serveHTTPOptions(cfg, stdout, true, true)
	if err != nil {
		return err
	}
	return httpservicepkg.Install(o)
}
func uninstallServeHTTP(cfg Config, stdout io.Writer) error {
	o, err := serveHTTPOptions(cfg, stdout, false, true)
	if err != nil {
		return err
	}
	return httpservicepkg.Uninstall(o)
}
func statusServeHTTP(cfg Config, stdout io.Writer) error {
	o, _ := serveHTTPOptions(cfg, stdout, false, false)
	return httpservicepkg.Status(o)
}
