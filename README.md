# gpu-autostop

运行在 Linux NVIDIA 物理机宿主机上，只监控实际出现过 NVIDIA GPU 进程的 Docker 容器。

当容器连续 `idle_timeout` 时间没有 NVIDIA compute 进程时停止容器。检测异常时本轮不会停止容器。

## 使用

先确认宿主机可以执行：

```bash
docker ps
nvidia-smi
```

程序不使用容器标签筛选范围，但普通容器不会被停止。容器首次出现 NVIDIA GPU 进程后才会进入监控；GPU 进程结束后，开始计算空闲时间。

默认是 `dry_run=true`，只记录将要停止的容器。确认日志无误后，将配置改为 `false`：

```json
"dry_run": false
```

编译和运行：

```bash
go build -o gpu-autostop .
./gpu-autostop -config config.json
```

第一版基于 `nvidia-smi` 的 compute 进程和 `/proc/<pid>/cgroup` 映射容器。暂不覆盖 MIG、MPS、图形进程和 NVML 深度统计。
