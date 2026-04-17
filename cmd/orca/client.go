package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/atterpac/orca/pkg/orca"
	"gopkg.in/yaml.v3"
)

func daemonAddr() string {
	if v := os.Getenv("ORCA_ADDR"); v != "" {
		return v
	}
	return "http://localhost:7878"
}

func httpDo(method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, daemonAddr()+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func runSpawn(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: orca spawn <file.yaml>")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var spec orca.AgentSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return fmt.Errorf("yaml: %w", err)
	}
	resp, err := httpDo("POST", "/agents", spec)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("spawn failed: %s", string(out))
	}
	fmt.Println(string(out))
	return nil
}

func runList(args []string) error {
	resp, err := httpDo("GET", "/agents", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var agents []orca.AgentInfo
	if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil {
		return err
	}
	if len(agents) == 0 {
		fmt.Println("no agents")
		return nil
	}
	fmt.Printf("%-20s %-12s %-10s %12s %12s %12s\n", "ID", "STATUS", "RUNTIME", "IN", "OUT", "COST_USD")
	for _, a := range agents {
		fmt.Printf("%-20s %-12s %-10s %12d %12d %12.4f\n",
			a.Spec.ID, a.Status, a.Spec.Runtime,
			a.Usage.InputTokens, a.Usage.OutputTokens, a.Usage.CostUSD)
	}
	return nil
}

func runUsage(args []string) error {
	resp, err := httpDo("GET", "/usage", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var v struct {
		Total    orca.TokenUsage   `json:"total"`
		PerAgent []orca.AgentInfo  `json:"per_agent"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return err
	}
	fmt.Printf("TOTAL  in=%d out=%d cache_create=%d cache_read=%d turns=%d cost=$%.4f\n",
		v.Total.InputTokens, v.Total.OutputTokens, v.Total.CacheCreationTokens,
		v.Total.CacheReadTokens, v.Total.Turns, v.Total.CostUSD)
	for _, a := range v.PerAgent {
		fmt.Printf("  %-20s in=%d out=%d cache_r=%d turns=%d cost=$%.4f\n",
			a.Spec.ID, a.Usage.InputTokens, a.Usage.OutputTokens,
			a.Usage.CacheReadTokens, a.Usage.Turns, a.Usage.CostUSD)
	}
	return nil
}

func runSend(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: orca send <agent_id> <text>")
	}
	id := args[0]
	text := strings.Join(args[1:], " ")
	body := map[string]any{
		"from": "cli",
		"kind": "request",
		"body": jsonString(text),
	}
	resp, err := httpDo("POST", "/agents/"+id+"/message", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("send failed: %s", string(out))
	}
	fmt.Println(string(out))
	return nil
}

func runKill(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: orca kill <agent_id>")
	}
	resp, err := httpDo("DELETE", "/agents/"+args[0], nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		out, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kill failed: %s", string(out))
	}
	fmt.Println("killed:", args[0])
	return nil
}

func runTail(args []string) error {
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	agent := fs.String("agent", "", "filter by agent id")
	kinds := fs.String("kinds", "", "comma-separated event kinds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := "/events"
	if *agent != "" {
		path = "/agents/" + *agent + "/events"
	}
	if *kinds != "" {
		path += "?kinds=" + *kinds
	}
	resp, err := http.Get(daemonAddr() + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			pretty := prettyEvent(data)
			fmt.Println(pretty)
		}
	}
	return scanner.Err()
}

func prettyEvent(data string) string {
	var ev orca.Event
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return data
	}
	ts := ev.Timestamp.Format("15:04:05.000")
	payload, _ := json.Marshal(ev.Payload)
	if len(payload) > 200 {
		payload = append(payload[:197], '.', '.', '.')
	}
	return fmt.Sprintf("%s [%-8s] %-16s %s", ts, ev.AgentID, ev.Kind, string(payload))
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
