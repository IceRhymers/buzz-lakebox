package main

import (
	"fmt"
	"os"
	"strings"
)

// runSelftest implements --selftest mode (§5C, plan step 3 "mux-selftest"):
// load the config, spawn + initialize every child, perform one internal
// tools/list merge, run all §5B validations, print the merged tool count to
// stderr, and exit 0 on success or non-zero with a clear message on any
// violation.  No agent stdin/stdout loop is started.
func runSelftest() {
	path, err := resolveConfigPath()
	if err != nil {
		logf("bzmux --selftest: resolve config path: %v\n", err)
		os.Exit(1)
	}
	logf("bzmux --selftest: loading config from %s\n", path)

	cfg, err := loadConfig(path)
	if err != nil {
		logf("bzmux --selftest: %v\n", err)
		os.Exit(1)
	}

	// Spawn and initialize all children.
	type childResult struct {
		c   *child
		err error
	}
	ch := make(chan childResult, len(cfg.Servers))

	// We need a minimal proxy for the inflight machinery used by childCall.
	// In selftest mode we don't connect to an agent, so agentIn/Out are nil.
	p := &proxy{
		cfg:           cfg,
		catalogReady:  make(chan struct{}),
		inflightByID:  make(map[int64]*inflightEntry),
		inflightByKey: make(map[string]int64),
	}

	protocolVersion := "2024-11-05"

	for _, srv := range cfg.Servers {
		srv := srv
		go func() {
			env := buildChildEnv(srv.Env)
			c, err := spawn(srv.Name, srv.Command, srv.Args, env)
			if err != nil {
				ch <- childResult{err: fmt.Errorf("spawn %q: %w", srv.Name, err)}
				return
			}
			go c.readLoop(p)
			if err := c.initialize(p, protocolVersion); err != nil {
				c.markDead(err)
				ch <- childResult{c: c, err: err}
				return
			}
			ch <- childResult{c: c}
		}()
	}

	var children []*child
	var errParts []string
	for range cfg.Servers {
		r := <-ch
		if r.err != nil {
			errParts = append(errParts, r.err.Error())
		}
		if r.c != nil {
			children = append(children, r.c)
		}
	}

	// Kill all spawned children on exit (selftest is a one-shot check).
	defer func() {
		for _, c := range children {
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
		}
	}()

	if len(errParts) > 0 {
		logf("bzmux --selftest: FAIL: child initialization errors:\n")
		for _, e := range errParts {
			logf("  - %s\n", e)
		}
		os.Exit(1)
	}

	// §5B validations: collision, __, 64-byte budget.
	_, catalog, err := mergeCatalogs(children)
	if err != nil {
		logf("bzmux --selftest: FAIL: catalog validation: %v\n", err)
		os.Exit(1)
	}

	// Check protocolVersion mismatches.
	var versionIssues []string
	for _, c := range children {
		if c.protocolVersion != "" && c.protocolVersion != protocolVersion {
			versionIssues = append(versionIssues, fmt.Sprintf(
				"child %q negotiated protocolVersion %q, bzmux offered %q",
				c.name, c.protocolVersion, protocolVersion))
		}
	}
	if len(versionIssues) > 0 {
		logf("bzmux --selftest: FAIL: protocolVersion mismatch:\n")
		for _, v := range versionIssues {
			logf("  - %s\n", v)
		}
		os.Exit(1)
	}

	// Check that the merged catalog JSON is valid (smoke check).
	if _, err := buildToolsListResult(catalog); err != nil {
		logf("bzmux --selftest: FAIL: build tools/list: %v\n", err)
		os.Exit(1)
	}

	// Success: print summary.
	names := make([]string, len(children))
	for i, c := range children {
		names[i] = fmt.Sprintf("%s (%d tools)", c.name, len(c.tools))
	}
	logf("bzmux --selftest: OK: %d tools merged from %d children: %s\n",
		len(catalog), len(children), strings.Join(names, ", "))
}
