package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/atterpac/orca/pkg/orca"
	"gopkg.in/yaml.v3"
)

// runValidate validates one or more agent yaml files. If the daemon is
// reachable the validation runs against live state (registered runtimes,
// existing agents, open tasks); otherwise it falls back to a schema-only
// check.
func runValidate(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: orca validate <file.yaml>...")
	}

	daemonReachable := false
	if resp, err := http.Get(daemonAddr() + "/healthz"); err == nil {
		resp.Body.Close()
		daemonReachable = resp.StatusCode == 200
	}
	if !daemonReachable {
		fmt.Fprintln(os.Stderr, "(daemon not reachable — schema-only checks)")
	}

	totalFatal := 0
	for _, path := range args {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: read failed: %v\n", path, err)
			totalFatal++
			continue
		}
		var spec orca.AgentSpec
		if err := yaml.Unmarshal(data, &spec); err != nil {
			fmt.Fprintf(os.Stderr, "%s: yaml parse failed: %v\n", path, err)
			totalFatal++
			continue
		}

		var errs []orca.ValidationError
		if daemonReachable {
			body, _ := json.Marshal(spec)
			resp, err := http.Post(daemonAddr()+"/validate/agent-spec", "application/json", bytes.NewReader(body))
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: validate request failed: %v\n", path, err)
				totalFatal++
				continue
			}
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var resBody struct {
				Errors []orca.ValidationError `json:"errors"`
			}
			_ = json.Unmarshal(out, &resBody)
			errs = resBody.Errors
		} else {
			errs = orca.ValidateSpec(spec, orca.ValidationContext{
				StrictRoleTemplate: os.Getenv("ORCA_REQUIRE_ROLE_TEMPLATE") == "1",
			})
		}

		fatal := orca.FatalCount(errs)
		if fatal == 0 && len(errs) == 0 {
			fmt.Printf("✓ %s (id=%q)\n", path, spec.ID)
			continue
		}
		if fatal > 0 {
			fmt.Printf("✗ %s (id=%q) — %d errors\n", path, spec.ID, fatal)
		} else {
			fmt.Printf("⚠ %s (id=%q) — %d warnings\n", path, spec.ID, len(errs))
		}
		for _, e := range errs {
			marker := "warn"
			if e.Fatal {
				marker = "ERR "
			}
			fmt.Printf("  [%s] %s: %s\n", marker, e.Field, e.Message)
		}
		totalFatal += fatal
	}

	if totalFatal > 0 {
		return fmt.Errorf("%d fatal validation error(s)", totalFatal)
	}
	return nil
}
