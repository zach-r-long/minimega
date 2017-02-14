// Copyright (2015) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"fmt"
	"minicli"
	"os"
	"path/filepath"
	"ron"
	"strconv"
	"strings"
)

var filter *ron.Filter

// wrapCLI wraps handlers that return a single response. This greatly reduces
// boilerplate code with minicli handlers.
func wrapCLI(fn func(*minicli.Command, *minicli.Response) error) minicli.CLIFunc {
	return func(c *minicli.Command, respChan chan<- minicli.Responses) {
		resp := &minicli.Response{Host: hostname}
		if err := fn(c, resp); err != nil {
			resp.Error = err.Error()
		}
		respChan <- minicli.Responses{resp}
	}
}

var cliHandlers = []minicli.Handler{
	{
		HelpShort: "list clients",
		Patterns: []string{
			"clients",
		},
		Call: wrapCLI(func(c *minicli.Command, resp *minicli.Response) error {
			resp.Header = []string{
				"UUID", "arch", "OS", "hostname", "IPs", "MACs",
			}

			for _, client := range rond.GetActiveClients() {
				row := []string{
					client.UUID,
					client.Arch,
					client.OS,
					client.Hostname,
					fmt.Sprintf("%v", client.IPs),
					fmt.Sprintf("%v", client.MACs),
				}

				resp.Tabular = append(resp.Tabular, row)
			}

			return nil
		}),
	},
	{
		HelpShort: "set filter for subsequent commands",
		Patterns: []string{
			"filter [filter]",
			"<clear,> filter",
		},
		Call: wrapCLI(func(c *minicli.Command, resp *minicli.Response) error {
			arg := c.StringArgs["filter"]

			if c.BoolArgs["clear"] {
				filter = nil
			} else if arg == "" {
				resp.Response = fmt.Sprintf("%#v", filter)
			} else if f, err := parseFilter(arg); err != nil {
				return err
			} else {
				filter = f
			}

			return nil
		}),
	},
	{
		HelpShort: "run a command",
		Patterns: []string{
			"<exec,> <command>...",
			"<bg,> <command>...",
		},
		Call: wrapCLI(func(c *minicli.Command, resp *minicli.Response) error {
			id := rond.NewCommand(&ron.Command{
				Command:    c.ListArgs["command"],
				Filter:     filter,
				Background: c.BoolArgs["bg"],
			})

			resp.Response = strconv.Itoa(id)
			return nil
		}),
	},
	{
		HelpShort: "list processes",
		Patterns: []string{
			"processes",
		},
		Call: wrapCLI(func(c *minicli.Command, resp *minicli.Response) error {
			resp.Header = []string{
				"UUID", "pid", "command",
			}

			for _, client := range rond.GetActiveClients() {
				for _, proc := range client.Processes {
					row := []string{
						client.UUID,
						strconv.Itoa(proc.PID),
						fmt.Sprintf("%v", proc.Command),
					}

					resp.Tabular = append(resp.Tabular, row)
				}
			}

			return nil
		}),
	},
	{
		HelpShort: "kill PID",
		Patterns: []string{
			"kill <PID>",
		},
		Call: wrapCLI(func(c *minicli.Command, resp *minicli.Response) error {
			pid, err := strconv.Atoi(c.StringArgs["PID"])
			if err != nil {
				return err
			}

			rond.NewCommand(&ron.Command{
				PID:    pid,
				Filter: filter,
			})

			return nil
		}),
	},
	{
		HelpShort: "kill by name",
		Patterns: []string{
			"killall <name>",
		},
		Call: wrapCLI(func(c *minicli.Command, resp *minicli.Response) error {
			rond.NewCommand(&ron.Command{
				KillAll: c.StringArgs["name"],
				Filter:  filter,
			})

			return nil
		}),
	},
	{
		HelpShort: "send files",
		HelpLong: `
Send one or more files. Supports globs such as:

	send foo*
`,
		Patterns: []string{
			"send <file>",
		},
		Call: wrapCLI(func(c *minicli.Command, resp *minicli.Response) error {
			cmd := &ron.Command{
				Filter: filter,
			}

			arg := c.StringArgs["file"]
			if !filepath.IsAbs(arg) {
				arg = filepath.Join(*f_path, arg)
			}
			arg = filepath.Clean(arg)

			if !strings.HasPrefix(arg, *f_path) {
				return fmt.Errorf("can only send files from %v", *f_path)
			}

			files, err := filepath.Glob(arg)
			if err != nil || len(files) == 0 {
				return fmt.Errorf("non-existent files %v", arg)
			}

			for _, f := range files {
				file, err := filepath.Rel(*f_path, f)
				if err != nil {
					return fmt.Errorf("unable to determine relative path to %v: %v", f, err)
				}

				fi, err := os.Stat(f)
				if err != nil {
					return err
				}

				perm := fi.Mode() & os.ModePerm
				cmd.FilesSend = append(cmd.FilesSend, &ron.File{
					Name: file,
					Perm: perm,
				})
			}

			rond.NewCommand(cmd)
			return nil
		}),
	},
	{
		HelpShort: "get files",
		Patterns: []string{
			"recv <file>",
		},
		Call: wrapCLI(func(c *minicli.Command, resp *minicli.Response) error {
			cmd := &ron.Command{
				Filter: filter,
			}
			cmd.FilesRecv = append(cmd.FilesRecv, &ron.File{
				Name: c.StringArgs["file"],
			})

			rond.NewCommand(cmd)

			return nil
		}),
	},
	{
		HelpShort: "quit",
		Patterns: []string{
			"quit",
		},
		Call: func(_ *minicli.Command, _ chan<- minicli.Responses) {
			os.Exit(0)
		},
	},
}

func parseFilter(s string) (*ron.Filter, error) {
	filter := &ron.Filter{}

	if s == "" {
		return nil, nil
	}

	parts := strings.SplitN(s, "=", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("malformed id=value pair: %v", s)
	}

	switch strings.ToLower(parts[0]) {
	case "uuid":
		filter.UUID = strings.ToLower(parts[1])
	case "hostname":
		filter.Hostname = parts[1]
	case "arch":
		filter.Arch = parts[1]
	case "os":
		filter.OS = parts[1]
	case "ip":
		filter.IP = parts[1]
	case "mac":
		filter.MAC = parts[1]
	default:
		return nil, fmt.Errorf("unknown filter `%v`", parts[0])
	}

	return filter, nil
}
