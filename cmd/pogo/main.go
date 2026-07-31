package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/amirhasanzadehpy/Pogo/internal/lsp"
	pythonworker "github.com/amirhasanzadehpy/Pogo/internal/python"
	"github.com/amirhasanzadehpy/Pogo/internal/schema"
	"github.com/tliron/commonlog"
	"github.com/tliron/commonlog/simple"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet(lsp.ServerName, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: pogo [options]")
		fmt.Fprintln(stderr, "Pogo LSP 3.16 server over stdio; stdout is reserved for protocol traffic.")
		fmt.Fprintln(stderr, "Run only against trusted projects: Django startup executes project code.")
		flags.PrintDefaults()
	}
	logPath := flags.String("log-file", "", "write server logs to this file instead of stderr")
	projectRoot := flags.String("project", "", "Django project root")
	pythonPath := flags.String("python", "", "Python interpreter for the Django worker")
	settingsModule := flags.String("settings", "", "Django settings module")
	showVersion := flags.Bool("version", false, "print the server version and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stderr, "%s %s\n", lsp.ServerName, lsp.ServerVersion)
		return 0
	}
	if err := configureLogging(*logPath); err != nil {
		fmt.Fprintf(stderr, "configure logging: %v\n", err)
		return 2
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := &schema.Cache{}
	features, err := lsp.NewFeatures(cache)
	if err != nil {
		fmt.Fprintf(stderr, "configure Python analysis: %v\n", err)
		return 2
	}
	defer features.Close()
	workerLogger := commonlog.GetLogger("django-worker")
	factory := func(params *protocol.InitializeParams) (lsp.Worker, error) {
		config, enabled, err := resolveWorkerConfig(*projectRoot, *pythonPath, *settingsModule, params)
		if err != nil || !enabled {
			return nil, err
		}
		return pythonworker.NewManager(config, cache, workerLogger)
	}
	return lsp.RunStdioWithFactory(ctx, cancel, factory, features)
}

type initializationOptions struct {
	DjangoORM struct {
		ProjectRoot    string `json:"projectRoot"`
		PythonPath     string `json:"pythonPath"`
		SettingsModule string `json:"settingsModule"`
	} `json:"djangoOrm"`
}

func resolveWorkerConfig(cliProject, cliPython, cliSettings string, params *protocol.InitializeParams) (pythonworker.Config, bool, error) {
	var options initializationOptions
	if params != nil && params.InitializationOptions != nil {
		payload, err := json.Marshal(params.InitializationOptions)
		if err != nil {
			return pythonworker.Config{}, false, fmt.Errorf("encode initialization options: %w", err)
		}
		if err := json.Unmarshal(payload, &options); err != nil {
			return pythonworker.Config{}, false, fmt.Errorf("decode initialization options: %w", err)
		}
	}

	projectRoot := firstNonempty(cliProject, options.DjangoORM.ProjectRoot)
	if projectRoot == "" && params != nil {
		if len(params.WorkspaceFolders) > 1 {
			return pythonworker.Config{}, false, fmt.Errorf("multiple workspace folders require djangoOrm.projectRoot or -project")
		}
		if len(params.WorkspaceFolders) == 1 {
			projectRoot = uriPath(string(params.WorkspaceFolders[0].URI))
		} else if params.RootURI != nil {
			projectRoot = uriPath(string(*params.RootURI))
		} else if params.RootPath != nil {
			projectRoot = *params.RootPath
		}
	}
	if projectRoot == "" {
		return pythonworker.Config{}, false, nil
	}
	projectRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return pythonworker.Config{}, false, fmt.Errorf("resolve project root: %w", err)
	}

	pythonPath := firstNonempty(cliPython, options.DjangoORM.PythonPath)
	if pythonPath == "" {
		if virtualEnvironment := os.Getenv("VIRTUAL_ENV"); virtualEnvironment != "" {
			pythonPath = virtualEnvironmentPython(virtualEnvironment)
		} else {
			candidate := virtualEnvironmentPython(filepath.Join(projectRoot, ".venv"))
			if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
				pythonPath = candidate
			}
		}
	}
	if pythonPath == "" {
		resolved, lookupErr := exec.LookPath("python3")
		if lookupErr != nil {
			pythonPath = "python3"
		} else {
			pythonPath = resolved
		}
	}
	settings := firstNonempty(cliSettings, options.DjangoORM.SettingsModule, os.Getenv("DJANGO_SETTINGS_MODULE"))
	return pythonworker.Config{
		ProjectRoot:    projectRoot,
		PythonPath:     pythonPath,
		SettingsModule: settings,
	}, true, nil
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func virtualEnvironmentPython(root string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(root, "Scripts", "python.exe")
	}
	return filepath.Join(root, "bin", "python")
}

func uriPath(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "" && parsed.Scheme != "file") {
		return ""
	}
	path := parsed.Path
	if parsed.Scheme == "" {
		path = value
	}
	if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	return filepath.FromSlash(strings.TrimSpace(path))
}

func configureLogging(path string) error {
	backend := simple.NewBackend()
	backend.Buffered = false
	commonlog.SetBackend(backend)

	if path == "" {
		commonlog.Configure(1, nil)
		return nil
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	commonlog.Configure(1, &path)
	return nil
}
