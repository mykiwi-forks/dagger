package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/Khan/genqlient/graphql"
	"github.com/containerd/containerd/platforms"
	bkclient "github.com/moby/buildkit/client"
	bkgw "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/util/progress/progressui"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"go.dagger.io/dagger/core"
	"go.dagger.io/dagger/core/schema"
	"go.dagger.io/dagger/internal/buildkitd"
	"go.dagger.io/dagger/project"
	"go.dagger.io/dagger/router"
	"go.dagger.io/dagger/sdk/go/dagger"
	"go.dagger.io/dagger/secret"
	"golang.org/x/sync/errgroup"
)

const (
	workdirID = "__dagger_workdir" // FIXME:(sipsma) just hoping users don't try to use this as an id themselves, not robust
	yamlName  = "cloak.yaml"
)

type Config struct {
	Workdir    string
	LocalDirs  map[string]string
	DevServer  int
	ConfigPath string
	// If true, just load extension metadata rather than compiling and stitching them in
	SkipInstall bool
}

type Context struct {
	context.Context
	Client     graphql.Client
	Workdir    core.DirectoryID
	LocalDirs  map[core.HostDirectoryID]core.DirectoryID
	Project    *schema.Project
	ConfigPath string
}

type StartCallback func(Context) error

func Start(ctx context.Context, startOpts *Config, fn StartCallback) error {
	if startOpts == nil {
		startOpts = &Config{}
	}

	c, err := buildkitd.Client(ctx)
	if err != nil {
		return err
	}

	platform, err := detectPlatform(ctx, c)
	if err != nil {
		return err
	}

	// FIXME:(sipsma) use viper to get env support automatically
	if startOpts.Workdir == "" {
		if v, ok := os.LookupEnv("DAGGER_WORKDIR"); ok {
			startOpts.Workdir = v
		}
	}
	if startOpts.ConfigPath == "" {
		if v, ok := os.LookupEnv("DAGGER_CONFIG"); ok {
			startOpts.ConfigPath = v
		}
	}

	switch {
	case startOpts.Workdir == "" && startOpts.ConfigPath == "":
		configAbsPath, err := findConfig()
		if err != nil {
			return err
		}
		startOpts.Workdir = filepath.Dir(configAbsPath)
		startOpts.ConfigPath = "./" + yamlName
	case startOpts.Workdir == "":
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		startOpts.Workdir = cwd
	case startOpts.ConfigPath == "":
		startOpts.ConfigPath = "./" + yamlName
	}

	router := router.New()
	secretStore := secret.NewStore()

	socketProviders := MergedSocketProviders{
		project.DaggerSockName: project.NewAPIProxy(router),
	}
	var sshAuthSockID string
	if _, ok := os.LookupEnv(sshAuthSockEnv); ok {
		sshAuthHandler, err := sshAuthSockHandler()
		if err != nil {
			return err
		}
		// using env key as the socket ID too for now
		sshAuthSockID = sshAuthSockEnv
		socketProviders[sshAuthSockID] = sshAuthHandler
	}
	solveOpts := bkclient.SolveOpt{
		Session: []session.Attachable{
			secretsprovider.NewSecretProvider(secretStore),
			socketProviders,
			authprovider.NewDockerAuthProvider(os.Stderr),
		},
	}
	if startOpts.LocalDirs == nil {
		startOpts.LocalDirs = map[string]string{}
	}
	startOpts.LocalDirs[workdirID] = startOpts.Workdir
	solveOpts.LocalDirs = startOpts.LocalDirs

	ch := make(chan *bkclient.SolveStatus)
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		var err error
		_, err = c.Build(ctx, solveOpts, "", func(ctx context.Context, gw bkgw.Client) (*bkgw.Result, error) {
			// Secret store is a circular dependency, since it needs to resolve
			// SecretIDs using the gateway, we don't have a gateway until we call
			// Build, which needs SolveOpts, which needs to contain the secret store.
			//
			// Thankfully we can just yeet the gateway into the store.
			secretStore.SetGateway(gw)

			coreAPI, err := schema.New(schema.InitializeArgs{
				Router:        router,
				SSHAuthSockID: sshAuthSockID,
				WorkdirID:     workdirID,
				Gateway:       gw,
				BKClient:      c,
				SolveOpts:     solveOpts,
				SolveCh:       ch,
				Platform:      *platform,
			})
			if err != nil {
				return nil, err
			}
			if err := router.Add(coreAPI); err != nil {
				return nil, err
			}

			ctx = withInMemoryAPIClient(ctx, router)
			engineCtx := Context{
				Context: ctx,
			}

			engineCtx.Client, err = dagger.Client(ctx)
			if err != nil {
				return nil, err
			}

			engineCtx.LocalDirs, err = loadLocalDirs(ctx, engineCtx.Client, solveOpts.LocalDirs)
			if err != nil {
				return nil, err
			}
			engineCtx.Workdir = engineCtx.LocalDirs[workdirID]
			engineCtx.ConfigPath = startOpts.ConfigPath

			engineCtx.Project, err = loadProject(
				ctx,
				engineCtx.Client,
				engineCtx.LocalDirs[workdirID],
				startOpts.ConfigPath,
				!startOpts.SkipInstall,
			)
			if err != nil {
				return nil, err
			}

			if fn == nil {
				return nil, nil
			}

			if err := fn(engineCtx); err != nil {
				return nil, err
			}

			if startOpts.DevServer != 0 {
				fmt.Fprintf(os.Stderr, "==> dev server listening on http://localhost:%d", startOpts.DevServer)
				return nil, http.ListenAndServe(fmt.Sprintf(":%d", startOpts.DevServer), router)
			}

			return bkgw.NewResult(), nil
		}, ch)
		return err
	})
	eg.Go(func() error {
		warn, err := progressui.DisplaySolveStatus(context.TODO(), "", nil, os.Stderr, ch)
		for _, w := range warn {
			fmt.Fprintf(os.Stderr, "=> %s\n", w.Short)
		}
		return err
	})
	if err := eg.Wait(); err != nil {
		return err
	}
	return nil
}

func withInMemoryAPIClient(ctx context.Context, router *router.Router) context.Context {
	return dagger.WithHTTPClient(ctx, &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				// TODO: not efficient, but whatever
				serverConn, clientConn := net.Pipe()

				go func() {
					_ = router.ServeConn(serverConn)
				}()

				return clientConn, nil
			},
		},
	})
}

func detectPlatform(ctx context.Context, c *bkclient.Client) (*specs.Platform, error) {
	w, err := c.ListWorkers(ctx)
	if err != nil {
		return nil, fmt.Errorf("error detecting platform %w", err)
	}

	if len(w) > 0 && len(w[0].Platforms) > 0 {
		dPlatform := w[0].Platforms[0]
		return &dPlatform, nil
	}
	defaultPlatform := platforms.DefaultSpec()
	return &defaultPlatform, nil
}

func loadLocalDirs(ctx context.Context, cl graphql.Client, localDirs map[string]string) (map[core.HostDirectoryID]core.DirectoryID, error) {
	var eg errgroup.Group
	var l sync.Mutex

	mapping := map[core.HostDirectoryID]core.DirectoryID{}
	for localID := range localDirs {
		localID := localID
		eg.Go(func() error {
			res := struct {
				Host struct {
					Directory struct {
						Read struct {
							ID core.DirectoryID `json:"id"`
						}
					}
				}
			}{}
			resp := &graphql.Response{Data: &res}

			err := cl.MakeRequest(ctx,
				&graphql.Request{
					Query: `
						query LocalDir($id: HostDirectoryID!) {
							host {
								directory(id: $id) {
									read {
										id
									}
								}
							}
						}
					`,
					Variables: map[string]any{
						"id": localID,
					},
				},
				resp,
			)
			if err != nil {
				return err
			}

			l.Lock()
			mapping[core.HostDirectoryID(localID)] = res.Host.Directory.Read.ID
			l.Unlock()

			return nil
		})
	}

	return mapping, eg.Wait()
}

func loadProject(ctx context.Context, cl graphql.Client, contextDir core.DirectoryID, configPath string, doInstall bool) (*schema.Project, error) {
	res := struct {
		Core struct {
			Directory struct {
				LoadProject schema.Project
			}
		}
	}{}
	resp := &graphql.Response{Data: &res}

	var install string
	if doInstall {
		install = "install"
	}

	err := cl.MakeRequest(ctx,
		&graphql.Request{
			// FIXME:(sipsma) toggling install is extremely weird here, need better way
			Query: fmt.Sprintf(`
			query LoadProject($dir: DirectoryID!, $configPath: String!) {
				directory(id: $dir) {
					loadProject(configPath: $configPath) {
						name
						schema
						extensions {
							path
							schema
							sdk
						}
						scripts {
							path
							sdk
						}
						dependencies {
							name
							schema
						}
						%s
					}
				}
			}`, install),
			Variables: map[string]any{
				"dir":        contextDir,
				"configPath": configPath,
			},
		},
		resp,
	)
	if err != nil {
		return nil, err
	}

	return &res.Core.Directory.LoadProject, nil
}

func findConfig() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	for {
		configPath := filepath.Join(wd, yamlName)
		// FIXME:(sipsma) decide how to handle symlinks
		if _, err := os.Stat(configPath); err == nil {
			return configPath, nil
		}

		if wd == "/" {
			return "", fmt.Errorf("no %s found", yamlName)
		}

		wd = filepath.Dir(wd)
	}
}