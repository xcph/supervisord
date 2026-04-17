# `container_run` 进程模型与 program 启动路径

本文描述 **Linux** 下本 fork 中 `container_run` **开启**与**关闭**时的进程关系，以及一次 **program** 从建命令到子进程跑起来的完整顺序。实现入口见：

- `process/process.go`（`createProgramCommand`、`setUser`、`run`）
- `process/namespace_linux.go`（`setContainerRun`、`createContainerRunWrapper`）
- `process/setenv_container_run_linux.go`（AppArmor 相关 env）
- `run_container_linux.go`、`run_apparmor_linux.go`、`main.go`（`__run_container__` / `__run_gate_exec__` / `__run_helper__`）

---

## 一、`container_run=false`（关闭）

### 进程模型

- **长期进程**：`supervisord` 主进程（加载配置、inet HTTP、进程表）。
- **每个 program**：**一个**直接子进程，即 `command=` 中的可执行文件及参数（例如 `node …`、`openclaw-init run …`），**不**再经 `supervisord` 二次包装。

```
supervisord ──fork/exec──► program（业务进程）
```

### program 启动全过程

1. **`Process.run()`**  
   状态切到 `Starting`，调用 `createProgramCommand()`，再 `cmd.Start()`。  
   `joinForegroundCpusetBeforeFork()` 仅在 `container_run=true` 时执行，此处跳过。

2. **`createProgramCommand()`**  
   - **`parseCommand`**：解析 `command=` 得到 `[]string`。  
   - **不**调用 `createContainerRunWrapper`，argv 保持原样。  
   - **`createCommand`**：`exec.Command(args[0])`，`cmd.Args = args`，新建 `cmd.SysProcAttr`。

3. **`setUser()`**（若配置了 `user=`）  
   - 解析 uid/gid。  
   - **`setUserID`**：在 `SysProcAttr.Credential` 中设置 `Uid`/`Gid`（`NoSetGroups: true`），由内核在子进程 **`exec` 时**完成降权。  
   - 合并/覆盖 `HOME`、`USER`、`LOGNAME`、`PATH`，并过滤部分 root 遗留环境变量。

4. **`setDeathsig`**  
   子进程在父进程异常退出时可收到信号（平台相关）。

5. **`setContainerRun`**  
   **空操作**（不设置 `Cloneflags`）。

6. **`setEnv()`**  
   合并 `environment=`、`envFiles` 与 `os.Environ()`。  
   **不会**注入 `SUPERVISORD_APPARMOR_*`、`SUPERVISORD_TARGET_*`（`maybeInjectContainerRunAppArmorEnv` 仅在 `container_run=true` 时调用）。

7. **`setDir` / `setLog` / stdin pipe**  
   与上游 supervisord 行为一致。

8. **`cmd.Start()`**  
   `fork` + `execve` 到配置中的程序。  
   **supervisord 记录的 program PID** = 该业务进程在外层 PID 命名空间中的 PID。

9. **监视与退出**  
   `waitForExit`、`monitorProgramIsRunning` 等针对上述子进程；**无** `__run_container__`、helper、gate 等中间层。

---

## 二、`container_run=true`（开启）

### 进程模型（概念）

- **supervisord** 拉起的**直接子进程**不是业务命令本身，而是 **`supervisord` 再执行自身**，参数为 **`__run_container__`**，并在 **`clone`/`fork`** 时带上若干命名空间标志（类轻量容器）。
- 在新 **PID namespace** 中，该 wrapper 通常为 **PID 1**，负责 mount、新 `/proc`、网络/bootstrap（视配置）、路径遮蔽等；随后按 **最终 target** 分支：
  - **目标为 `/init` 或 `/sbin/init`（Android）**：先 **ForkExec** `supervisord __run_helper__`；当前 **PID 1** 再（可选）AppArmor、`setuid` 后 **`exec` `/init`**。
  - **其它目标（如 OpenClaw 网关）**：先 **ForkExec** **`__run_gate_exec__`**（在新 ns 中多为 **PID 2**），由其写 AppArmor `attr/exec` 并 **`exec` 真实网关**；再 **ForkExec** `__run_helper__`；**PID 1** 进入 **`wait4(-1)`** 循环回收子进程，**不再** `exec` 成业务进程。

非 `/init` 时示意：

```
supervisord
  └── [clone: NEWPID | NEWNS | NEWCGROUP | NEWUTS | NEWIPC (+ 可选 NEWNET)]
        └── PID 1: supervisord __run_container__ …（wrapper / 小 init）
              ├── PID 2: supervisord __run_gate_exec__ … → exec → 真实网关（如 node）
              └── PID n: supervisord __run_helper__ …（延迟任务、长期存活）
```

**supervisord 在 program 维度记录的 PID** 是 **wrapper 在外层命名空间中的 PID**（`cmd.Start()` 对应进程），**不是** `exec` 之后业务进程在新 PID ns 内的编号；外层 `ps` 与容器内 `ps` 对照时需注意 PID namespace 映射。

### program 启动全过程

#### 阶段 A：supervisord 侧

1. **`createProgramCommand()`**  
   - 将 `command` 经 **`createContainerRunWrapper`** 改为：  
     `[getSupervisordPath(), "__run_container__", 原 argv0, 原 argv1, …]`  
   - 子进程镜像为 **supervisord**，在 `main` 中进入 **`handleRunContainer()`**。

2. **`setUser()`**（若配置了 `user=`）  
   - **不**设置 `SysProcAttr.Credential`（不在首次 `clone` 时降权）。  
   - 写入 **`SUPERVISORD_TARGET_UID` / `SUPERVISORD_TARGET_GID`**，将 **`setuid`/`setgid` 推迟到 wrapper 最终 `exec` 前**（或 gate 子进程内），以便 wrapper 以 **root** 完成 mount 等。

3. **`setContainerRun`**  
   在 `SysProcAttr.Cloneflags` 上追加：  
   `CLONE_NEWPID | CLONE_NEWNS | CLONE_NEWCGROUP | CLONE_NEWUTS | CLONE_NEWIPC`  
   若 `container_network_isolated=true`，再追加 **`CLONE_NEWNET`**（OpenClaw 默认栈多为 false，与 pod/宿主机共享网络栈）。

4. **`setEnv()`**  
   - 若配置了 `container_run_apparmor_profile` 且 **`AppArmorExecTransitionAvailable()`** 为真，注入 **`SUPERVISORD_APPARMOR_EXEC_PROFILE`**（及可选 **`SUPERVISORD_APPARMOR_RELAXED`**）。  
   - 将当前 **`/etc/resolv.conf`** 写入 **`/shared/etc/resolv.conf`**，供隔离 mount 命名空间内使用。

5. **`run()` 中 `cmd.Start()` 之前**  
   **`joinForegroundCpusetBeforeFork()`**（仅 `container_run=true`）：在 `clone` 前把当前任务绑到预期 cpuset（Android/redroid 场景）。

6. **`cmd.Start()`**  
   使用上述 `Cloneflags` 创建子进程：首个进程在新 PID ns 中为 **PID 1**，执行 **`supervisord __run_container__ …`**。

#### 阶段 B：`__run_container__`（新 namespace 内的 PID 1）

`handleRunContainer()` 前半段对所有 target 大致相同，包括：

1. 根下挂载递归 **`MS_PRIVATE`**。  
2. **卸载并重新挂载 `/proc`**（适配新 PID ns），必要时 **`MS_REMOUNT` rw**。  
3. **`setupLoopback`**（若使用独立 network ns）。  
4. **CNI 快照/恢复**、`cloudphoneNetworkBootstrap`（若启用）。  
5. **`maskPathsForRunContainerAndroid`** 等。  
6. **`setupCpusetForRedroid`**、`unmountForInitRestart`、`mountSysfsWithSimulatedCpu` 等。

之后按 **target** 分支：

**B1. `/init` 或 `/sbin/init`（Android）**

- **ForkExec** `supervisord __run_helper__ …`：子进程执行 **`runHelperDelayedTasks()`**（定时 `restorePodNetwork`、`procfs-simulator`、墓碑目录等），通常 **长期不退出**。  
- **当前 PID 1**：`prepareAppArmorExecTransition(true)`（若仍为 PID 1 则可能 **跳过** 写 `/proc/self/attr/exec`）、**`dropPrivilegesForFinalExecIfRequested()`**、**`filterSupervisordInternalEnv`** 后 **`unix.Exec(target, argv, env)`** → 成为 **真实 `/init`**。

**B2. 非 init（例如 OpenClaw 网关）**

- **先** **ForkExec** `supervisord __run_gate_exec__ <target> …` → 新 ns 内多为 **PID 2**。  
- **再** **ForkExec** `supervisord __run_helper__ …`。  
- **PID 1** 进入 **`runContainerChildReaperLoop()`**：**`wait4(-1, …)`** 循环回收子进程；**不**再 `exec` 业务程序。

**`__run_gate_exec__` 子进程**

- `prepareAppArmorExecTransition(false)`：**不因「PID 1」而跳过**，向 **`/proc/self/attr/exec`** 写入 `exec <profile>`，使 **下一次 `execve`** 进入配置的 AppArmor profile。  
- **`dropPrivilegesForFinalExecIfRequested()`**  
- **`filterSupervisordInternalEnv`** 后 **`unix.Exec(target, argv, env)`** → 变为 **真实网关进程**（如 `openclaw-gateway-exec.sh` → `node`）。

> **说明**：AppArmor 的 `exec` 过渡绑定在「下一次 `execve`」。若先 **ForkExec** helper，会 **消费** 掉已排队的 transition；且对 **PID 1** 写 `attr/exec` 常被跳过或失败。因此对非 init 目标采用 **gate 子进程（pid≠1）** 排队 profile 再 `exec` 网关；PID 1 专职 **reap**。

---

## 三、对照表

| 项目 | `container_run=false` | `container_run=true` |
|------|------------------------|------------------------|
| 子进程 argv | 配置中的 `command` 原样 | `supervisord __run_container__ …` |
| `clone` 命名空间 | 无（与 supervisord 同套 ns） | NEWPID + NEWNS + CGROUP + UTS + IPC（可选 NEWNET） |
| `user=` 生效方式 | `SysProcAttr.Credential`（fork/exec 时降权） | 环境变量 `SUPERVISORD_TARGET_*`，在最终 `exec` 前（或 gate 内）`setuid` |
| AppArmor exec profile | 不经由 `SUPERVISORD_APPARMOR_*` | 可注入 env；**非 init** 在 **`__run_gate_exec__`** 写 `attr/exec` |
| supervisord 记录的 PID | 业务进程 | **wrapper**（新 PID ns 的 1 号，在外层有一个 PID） |
| 典型额外进程 | 无 | **helper**；非 init 时还有 **gate → exec 后的业务进程**；PID 1 **wait** 回收 |

---

## 四、信号与 stop/kill（简要）

`stopasgroup` / `killasgroup` 发送信号时，目标一般是 **supervisord 记录的 `p.cmd.Process`**，即 **wrapper 外层 PID**（`container_run=true` 时）。具体是否整组退出取决于该 PID 与业务进程、helper 的进程组/session 关系；排障时可结合 `ps -eo pid,ppid,pgid,sid,cmd` 与 `supervisord ctl status` 对照。

---

*文档与当前 `main` / `run_container_linux.go` / `run_apparmor_linux.go` 实现同步；若后续调整分支条件或命名空间标志，请一并更新本文。*
