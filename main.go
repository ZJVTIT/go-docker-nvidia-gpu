package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	CheckInterval time.Duration
	IdleTimeout   time.Duration
	StopTimeout   int
	DryRun        bool
}

type Container struct {
	ID   string
	Name string
}

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	if cfg.CheckInterval <= 0 || cfg.IdleTimeout <= 0 || cfg.StopTimeout < 0 {
		log.Fatal("配置无效：时间必须为正数，停止超时不能为负数")
	}

	log.Printf("gpu-autostop started: monitor=all-containers interval=%s idle_timeout=%s dry_run=%t", cfg.CheckInterval, cfg.IdleTimeout, cfg.DryRun)
	ticker := time.NewTicker(cfg.CheckInterval)
	defer ticker.Stop()

	idleSince := make(map[string]time.Time)
	tracked := make(map[string]bool)
	for {
		if err := checkOnce(cfg, idleSince, tracked); err != nil {
			// 检测异常时只记录，不停止任何容器。
			log.Printf("检测失败，本轮不执行停止操作: %v", err)
		}
		<-ticker.C
	}
}

func loadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置文件: %w", err)
	}
	var raw struct {
		CheckInterval string `json:"check_interval"`
		IdleTimeout   string `json:"idle_timeout"`
		StopTimeout   int    `json:"stop_timeout_seconds"`
		DryRun        bool   `json:"dry_run"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Config{}, fmt.Errorf("解析配置文件: %w", err)
	}
	checkInterval, err := time.ParseDuration(raw.CheckInterval)
	if err != nil {
		return Config{}, fmt.Errorf("解析 check_interval: %w", err)
	}
	idleTimeout, err := time.ParseDuration(raw.IdleTimeout)
	if err != nil {
		return Config{}, fmt.Errorf("解析 idle_timeout: %w", err)
	}
	return Config{CheckInterval: checkInterval, IdleTimeout: idleTimeout, StopTimeout: raw.StopTimeout, DryRun: raw.DryRun}, nil
}

func checkOnce(cfg Config, idleSince map[string]time.Time, tracked map[string]bool) error {
	containers, err := listContainers()
	if err != nil {
		return err
	}
	gpuPIDs, err := listGPUProcesses()
	if err != nil {
		return err
	}

	now := time.Now()
	active := make(map[string]bool)
	for _, c := range containers {
		used, err := containerUsesGPU(c, gpuPIDs)
		if err != nil {
			return fmt.Errorf("容器 %s: %w", c.Name, err)
		}
		if used {
			active[c.ID] = true
			tracked[c.ID] = true
			delete(idleSince, c.ID)
			log.Printf("container=%s gpu=active", c.Name)
			continue
		}
		// 从未发现 GPU 进程的容器不纳入监控，也不会被停止。
		if !tracked[c.ID] {
			continue
		}

		start, ok := idleSince[c.ID]
		if !ok {
			idleSince[c.ID] = now
			log.Printf("container=%s gpu=idle, 开始计时", c.Name)
			continue
		}
		if now.Sub(start) < cfg.IdleTimeout {
			log.Printf("container=%s gpu=idle idle_for=%s", c.Name, now.Sub(start).Round(time.Second))
			continue
		}

		if cfg.DryRun {
			log.Printf("DRY-RUN: container=%s 将停止（已空闲 %s）", c.Name, now.Sub(start).Round(time.Second))
			delete(idleSince, c.ID)
			continue
		}
		if err := stopContainer(c, cfg.StopTimeout); err != nil {
			return err
		}
		log.Printf("container=%s 已停止", c.Name)
		delete(idleSince, c.ID)
	}

	for id := range idleSince {
		if !active[id] && !containsContainer(containers, id) {
			delete(idleSince, id)
		}
	}
	for id := range tracked {
		if !containsContainer(containers, id) {
			delete(tracked, id)
		}
	}
	return nil
}

func listContainers() ([]Container, error) {
	out, err := command("docker", "ps", "--no-trunc", "--format", "{{.ID}}|{{.Names}}")
	if err != nil {
		return nil, fmt.Errorf("查询 Docker 容器: %w", err)
	}
	var result []Container
	for _, line := range nonEmptyLines(out) {
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("Docker 输出格式异常: %q", line)
		}
		result = append(result, Container{ID: parts[0], Name: parts[1]})
	}
	return result, nil
}

func listGPUProcesses() ([]string, error) {
	out, err := command("nvidia-smi", "--query-compute-apps=pid", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, fmt.Errorf("查询 nvidia-smi: %w", err)
	}
	var pids []string
	r := csv.NewReader(strings.NewReader(out))
	for {
		record, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("解析 nvidia-smi 输出: %w", err)
		}
		if len(record) > 0 && strings.TrimSpace(record[0]) != "" {
			pids = append(pids, strings.TrimSpace(record[0]))
		}
	}
	return pids, nil
}

func containerUsesGPU(c Container, pids []string) (bool, error) {
	for _, pid := range pids {
		data, err := os.ReadFile(filepath.Join("/proc", pid, "cgroup"))
		if os.IsNotExist(err) {
			continue // 进程可能在 nvidia-smi 和读取 /proc 之间退出。
		}
		if err != nil {
			return false, fmt.Errorf("读取 GPU 进程 %s 的 cgroup: %w", pid, err)
		}
		if strings.Contains(string(data), c.ID) {
			return true, nil
		}
	}
	return false, nil
}

func stopContainer(c Container, timeout int) error {
	if _, err := command("docker", "stop", "--time", fmt.Sprint(timeout), c.ID); err != nil {
		return fmt.Errorf("停止容器 %s: %w", c.Name, err)
	}
	return nil
}

func command(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func nonEmptyLines(s string) []string {
	var result []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}

func containsContainer(containers []Container, id string) bool {
	for _, c := range containers {
		if c.ID == id {
			return true
		}
	}
	return false
}
