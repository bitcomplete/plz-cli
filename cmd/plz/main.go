package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"

	"github.com/bitcomplete/plz-cli/client/actions"
	"github.com/bitcomplete/plz-cli/client/deps"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	"github.com/zalando/go-keyring"
)

var Version = "dev"

func main() {
	app := &cli.App{
		Version: Version,
		Usage:   "plz.review command-line companion",
		Commands: []*cli.Command{
			{
				Name:   "auth",
				Usage:  "authorize GitHub access",
				Action: actions.Auth,
			},
			{
				Name:   "review",
				Usage:  "start a review",
				Action: actions.Review,
				Flags: []cli.Flag{
					&cli.StringSliceFlag{
						Name:    "reviewer",
						Aliases: []string{"r"},
						Usage:   "add reviewer by GitHub username",
					},
				},
			},
			{
				Name:   "switch",
				Usage:  "switch to a review branch",
				Action: actions.Switch,
			},
		},
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "verbose",
				Usage: "show verbose debug output",
			},
			&cli.StringFlag{
				Name:  "plz-api-url",
				Value: "https://api.plz.review/api/v1",
				Usage: "point to a different plz server",
			},
		},
		Before: func(c *cli.Context) error {
			d, err := makeDeps(c)
			if err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				// Don't go through ExitErrHandler because it requires deps.
				os.Exit(1)
			}
			c.Context = deps.ContextWithDeps(c.Context, d)
			return nil
		},
		ExitErrHandler: func(c *cli.Context, err error) {
			deps := deps.FromContext(c.Context)
			if err != nil {
				deps.ErrorLog.Println(err.Error())
				var stackTracer interface {
					StackTrace() errors.StackTrace
				}
				if errors.As(err, &stackTracer) {
					deps.DebugLog.Printf("%+v", stackTracer.StackTrace())
				}
				os.Exit(1)
			}
		},
	}
	_ = app.Run(os.Args)
}

func makeDeps(c *cli.Context) (*deps.Deps, error) {
	debugWriter := ioutil.Discard
	if c.Bool("verbose") {
		debugWriter = os.Stdout
	}
	authToken, err := keyring.Get("plz", "default")
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return nil, errors.Wrap(err, "error accessing keychain")
	}
	return &deps.Deps{
		ErrorLog:  log.New(os.Stderr, "", 0),
		InfoLog:   log.New(os.Stdout, "", 0),
		DebugLog:  log.New(debugWriter, "[debug] ", log.Ldate|log.Lmicroseconds),
		AuthToken: authToken,
		PlzAPIURL: c.String("plz-api-url"),
	}, nil
}
