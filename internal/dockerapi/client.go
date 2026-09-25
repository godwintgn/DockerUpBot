package dockerapi

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Client talks to the Docker Engine API over DOCKER_HOST (default unix socket).
type Client struct {
	http    *http.Client
	baseURL string
}

func New() (*Client, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	c := &Client{}
	switch {
	case strings.HasPrefix(host, "unix://"):
		sock := strings.TrimPrefix(host, "unix://")
		c.baseURL = "http://docker"
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}
		c.http = &http.Client{Transport: tr, Timeout: 0}
	case strings.HasPrefix(host, "tcp://"):
		c.baseURL = "http://" + strings.TrimPrefix(host, "tcp://")
		c.http = &http.Client{Timeout: 0}
	case strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://"):
		c.baseURL = strings.TrimRight(host, "/")
		c.http = &http.Client{Timeout: 0}
	default:
		return nil, fmt.Errorf("unsupported DOCKER_HOST %q", host)
	}
	slog.Info("docker api client ready", "host", host, "api", "v1.44")
	return c, nil
}

func (c *Client) Close() error { return nil }

func (c *Client) api(path string) string {
	return c.baseURL + "/v1.44" + path
}

// PullState aggregates layer pull progress.
type PullState struct {
	Status     string
	ID         string
	Current    int64
	Total      int64
	Done       int
	Active     int
	Waiting    int
	SpeedBps   float64
	ETA        time.Duration
	StartedAt  time.Time
	BytesSoFar int64
}

type layerProg struct {
	Status  string
	Current int64
	Total   int64
}

type ContainerInfo struct {
	ID     string
	Image  string
	Name   string
	Config ContainerConfig
	State  ContainerState
}

type ContainerConfig struct {
	Image  string
	Labels map[string]string
}

type ContainerState struct {
	Status  string
	Running bool
	Health  *struct {
		Status string
	}
}

// PullImage pulls an image and reports aggregated progress via cb.
func (c *Client) PullImage(ctx context.Context, ref string, cb func(PullState)) error {
	slog.Info("docker pull request", "image", ref)
	q := url.Values{"fromImage": {ref}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api("/images/create?")+q.Encode(), nil)
	if err != nil {
		return err
	}
	if auth := registryAuthHeader(ref); auth != "" {
		req.Header.Set("X-Registry-Auth", auth)
		slog.Info("docker pull using registry auth from docker config", "image", ref)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("docker pull returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 4096), 1024*1024)
	layers := map[string]layerProg{}
	start := time.Now()
	var lastBytes int64
	lastT := start
	var lastSpeed float64

	for sc.Scan() {
		line := sc.Bytes()
		var x struct {
			Status         string `json:"status"`
			ID             string `json:"id"`
			ProgressDetail struct {
				Current int64 `json:"current"`
				Total   int64 `json:"total"`
			} `json:"progressDetail"`
			Error string `json:"error"`
		}
		if json.Unmarshal(line, &x) != nil {
			continue
		}
		if x.Error != "" {
			return fmt.Errorf("%s", x.Error)
		}
		if x.ID != "" {
			lp := layers[x.ID]
			lp.Status = x.Status
			if x.ProgressDetail.Total > 0 {
				lp.Current = x.ProgressDetail.Current
				lp.Total = x.ProgressDetail.Total
			}
			layers[x.ID] = lp
		}

		var cur, tot int64
		done, active, waiting := 0, 0, 0
		for _, v := range layers {
			tot += v.Total
			cur += v.Current
			switch {
			case v.Status == "Pull complete" || v.Status == "Already exists":
				done++
			case v.Status == "Downloading" || v.Status == "Extracting":
				active++
			case v.Status == "Waiting" || v.Status == "Pulling fs layer":
				waiting++
			}
		}

		now := time.Now()
		dt := now.Sub(lastT).Seconds()
		speed := lastSpeed
		if dt > 0.2 {
			speed = float64(cur-lastBytes) / dt
			lastBytes = cur
			lastT = now
			lastSpeed = speed
		}
		eta := time.Duration(0)
		if speed > 0 && tot > cur {
			eta = time.Duration(float64(tot-cur)/speed) * time.Second
		}
		if cb != nil {
			cb(PullState{
				Status: x.Status, ID: x.ID, Current: cur, Total: tot,
				Done: done, Active: active, Waiting: waiting,
				SpeedBps: speed, ETA: eta, StartedAt: start, BytesSoFar: cur,
			})
		}
	}
	return sc.Err()
}

func (c *Client) InspectContainer(ctx context.Context, name string) (ContainerInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api("/containers/"+url.PathEscape(name)+"/json"), nil)
	if err != nil {
		return ContainerInfo{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ContainerInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ContainerInfo{}, fmt.Errorf("container %q not found (Docker HTTP 404) — check Diun ctn_names / dubot --container matches a running container name", name)
	}
	if resp.StatusCode >= 300 {
		return ContainerInfo{}, fmt.Errorf("inspect %q returned HTTP %d", name, resp.StatusCode)
	}
	var raw struct {
		Id     string `json:"Id"`
		Name   string `json:"Name"`
		Image  string `json:"Image"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Status  string `json:"Status"`
			Running bool   `json:"Running"`
			Health  *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return ContainerInfo{}, err
	}
	info := ContainerInfo{
		ID:    raw.Id,
		Image: raw.Image,
		Name:  strings.TrimPrefix(raw.Name, "/"),
		Config: ContainerConfig{
			Image:  raw.Config.Image,
			Labels: raw.Config.Labels,
		},
		State: ContainerState{
			Status:  raw.State.Status,
			Running: raw.State.Running,
		},
	}
	if raw.State.Health != nil {
		info.State.Health = &struct{ Status string }{Status: raw.State.Health.Status}
	}
	return info, nil
}

type ListedContainer struct {
	Name   string
	Image  string
	Labels map[string]string
}

// ListContainers returns containers matching a Docker filters JSON object (may be empty).
func (c *Client) ListContainers(ctx context.Context, filters string) ([]ListedContainer, error) {
	q := url.Values{"all": {"1"}}
	if filters != "" {
		q.Set("filters", filters)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api("/containers/json?")+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("list containers HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var raw []struct {
		Names  []string          `json:"Names"`
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
		State  string            `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]ListedContainer, 0, len(raw))
	for _, r := range raw {
		name := ""
		if len(r.Names) > 0 {
			name = strings.TrimPrefix(r.Names[0], "/")
		}
		out = append(out, ListedContainer{Name: name, Image: r.Image, Labels: r.Labels})
	}
	return out, nil
}

// FindRunningByImage returns one container whose image ref matches image.
func (c *Client) FindRunningByImage(ctx context.Context, image string) (string, error) {
	list, err := c.ListContainers(ctx, `{"status":["running"]}`)
	if err != nil {
		return "", err
	}
	for _, ct := range list {
		if sameImageRef(ct.Image, image) && ct.Name != "" {
			return ct.Name, nil
		}
	}
	return "", nil
}

// FindComposeContainer returns the container for a Compose project/service.
func (c *Client) FindComposeContainer(ctx context.Context, project, service string) (string, error) {
	if project == "" || service == "" {
		return "", nil
	}
	filters := fmt.Sprintf(`{"label":["com.docker.compose.project=%s","com.docker.compose.service=%s"]}`, project, service)
	list, err := c.ListContainers(ctx, filters)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", nil
	}
	return list[0].Name, nil
}

func sameImageRef(a, b string) bool {
	return normImage(a) == normImage(b) && normImage(a) != ""
}

func normImage(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "docker.io/")
	s = strings.TrimPrefix(s, "library/")
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	return s
}

func (c *Client) ContainerHealth(ctx context.Context, name string) (string, error) {
	info, err := c.InspectContainer(ctx, name)
	if err != nil {
		return "", err
	}
	if info.State.Health != nil && info.State.Health.Status != "" {
		return info.State.Health.Status, nil
	}
	if info.State.Running {
		return "running", nil
	}
	return info.State.Status, nil
}

func (c *Client) ContainerLogsTail(ctx context.Context, name string, n int) (string, error) {
	if n <= 0 {
		n = 30
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {fmt.Sprintf("%d", n)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api("/containers/"+url.PathEscape(name)+"/logs?")+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("logs returned HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if err != nil {
		return "", err
	}
	return stripDockerLogHeader(b), nil
}

// FollowLogs streams container logs from since onward and calls onLine for each text line.
// It blocks until ctx is cancelled or the stream ends; callers typically loop with backoff.
func (c *Client) FollowLogs(ctx context.Context, name string, since time.Time, onLine func(string)) error {
	q := url.Values{
		"stdout":     {"1"},
		"stderr":     {"1"},
		"follow":     {"1"},
		"timestamps": {"0"},
	}
	if !since.IsZero() {
		q.Set("since", strconv.FormatInt(since.Unix(), 10))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api("/containers/"+url.PathEscape(name)+"/logs?")+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("follow logs %q returned HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return readDockerLogStream(resp.Body, onLine)
}

func readDockerLogStream(r io.Reader, onLine func(string)) error {
	br := bufio.NewReader(r)
	var line strings.Builder
	flush := func() {
		if line.Len() == 0 {
			return
		}
		s := strings.TrimRight(line.String(), "\r\n")
		line.Reset()
		if s != "" && onLine != nil {
			onLine(s)
		}
	}
	hdr := make([]byte, 8)
	for {
		_, err := io.ReadFull(br, hdr)
		if err != nil {
			flush()
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		size := int(hdr[4])<<24 | int(hdr[5])<<16 | int(hdr[6])<<8 | int(hdr[7])
		if size < 0 || size > 1024*1024 {
			return fmt.Errorf("invalid docker log frame size %d", size)
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(br, payload); err != nil {
			return err
		}
		for _, b := range payload {
			if b == '\n' {
				flush()
				continue
			}
			line.WriteByte(b)
		}
	}
}

func stripDockerLogHeader(b []byte) string {
	var out strings.Builder
	i := 0
	for i+8 <= len(b) {
		size := int(b[i+4])<<24 | int(b[i+5])<<16 | int(b[i+6])<<8 | int(b[i+7])
		i += 8
		if size < 0 || i+size > len(b) {
			out.Write(b[i:])
			break
		}
		out.Write(b[i : i+size])
		i += size
	}
	if out.Len() == 0 {
		return string(b)
	}
	return out.String()
}

// TagImage tags source (id or name) as targetRef (name:tag).
func (c *Client) TagImage(ctx context.Context, source, targetRef string) error {
	repo, tag := splitImageRef(targetRef)
	if repo == "" {
		return fmt.Errorf("invalid tag target %q", targetRef)
	}
	if tag == "" {
		tag = "latest"
	}
	q := url.Values{"repo": {repo}, "tag": {tag}}
	path := c.api("/images/" + url.PathEscape(source) + "/tag?" + q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("tag %s -> %s:%s returned HTTP %d: %s", source, repo, tag, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	slog.Info("docker image tagged", "source", source, "repo", repo, "tag", tag)
	return nil
}

// PruneDanglingImages deletes unused dangling images. Returns reclaimed bytes when known.
func (c *Client) PruneDanglingImages(ctx context.Context) (int64, error) {
	q := url.Values{"filters": {`{"dangling":{"true":true}}`}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api("/images/prune?")+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, fmt.Errorf("prune returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		SpaceReclaimed int64 `json:"SpaceReclaimed"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	slog.Info("docker dangling images pruned", "reclaimed_bytes", out.SpaceReclaimed)
	return out.SpaceReclaimed, nil
}

func splitImageRef(ref string) (repo, tag string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", ""
	}
	// strip digest
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		// avoid splitting registry port host:5000/foo
		after := ref[i+1:]
		if !strings.Contains(after, "/") {
			return ref[:i], after
		}
	}
	return ref, "latest"
}

func registryAuthHeader(imageRef string) string {
	cfgPath := os.Getenv("DOCKER_CONFIG")
	if cfgPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		cfgPath = filepath.Join(home, ".docker")
	}
	b, err := os.ReadFile(filepath.Join(cfgPath, "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	host := registryHost(imageRef)
	candidates := []string{host, "https://" + host, "http://" + host, "https://index.docker.io/v1/"}
	for _, key := range candidates {
		if ac, ok := cfg.Auths[key]; ok {
			if ac.Auth != "" {
				return ac.Auth
			}
			if ac.Username != "" {
				raw := ac.Username + ":" + ac.Password
				return base64.StdEncoding.EncodeToString([]byte(raw))
			}
		}
	}
	for k, ac := range cfg.Auths {
		if strings.Contains(k, host) || strings.Contains(host, strings.TrimPrefix(strings.TrimPrefix(k, "https://"), "http://")) {
			if ac.Auth != "" {
				return ac.Auth
			}
			if ac.Username != "" {
				return base64.StdEncoding.EncodeToString([]byte(ac.Username + ":" + ac.Password))
			}
		}
	}
	return ""
}

func registryHost(ref string) string {
	ref = strings.TrimPrefix(ref, "https://")
	ref = strings.TrimPrefix(ref, "http://")
	parts := strings.Split(ref, "/")
	if len(parts) == 0 {
		return "docker.io"
	}
	first := parts[0]
	if !strings.Contains(first, ".") && !strings.Contains(first, ":") && first != "localhost" {
		return "docker.io"
	}
	return first
}
