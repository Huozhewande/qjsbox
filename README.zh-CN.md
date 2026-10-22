# qjsbox

[English](README.md)

在 Go 服务里安全地执行不可信的 JavaScript 规则，每次调用起一个独立的沙箱进程。

适合这样的场景：产品允许运营或客户自己写一小段逻辑，比如风控规则、打分因子、路由条件，服务端需要执行这段逻辑，但不能信任它。每次求值都会启动一个全新的原生 worker（基于 [QuickJS](https://bellard.org/quickjs/)）。worker 先解析脚本并按封闭的语法白名单检查，再在冻结的内置对象上执行，执行环境里没有时钟、随机数、模块、文件和网络，执行完立即退出。Go 侧负责截止时间、大小限制和并发上限。

## 为什么不用进程内解释器

goja、otto 这类进程内解释器和你的服务共用一个地址空间，解释器的 bug 或无界内存分配会波及整个服务。qjsbox 把每次求值都放到操作系统进程边界之后，并叠加几层互相独立的限制：

| 层 | 做什么 |
|-|-|
| 静态分析 | 执行前用 Acorn 遍历 AST，拒绝模块、`async`/生成器、`with`、隐式 `arguments` 和 `factors[...]` 这类动态访问 |
| 语言层 | 冻结全局对象和内置原型；移除 `Function`、`eval`、`Date`、`Math.random`；输入是深度冻结的只读代理 |
| 引擎层 | QuickJS 堆上限 64 MiB，JS 栈 512 KiB，900 ms 截止的中断钩子 |
| 进程层 | 每次调用一个新进程，关闭继承的描述符，禁止 core dump 和写文件，CPU 1 秒 |
| Linux（x86-64、AArch64） | 地址空间硬上限 256 MiB，`no_new_privs`，校验架构的 seccomp 白名单（13 个系统调用），打开文件、socket、`fork`、`exec` 一律拒绝 |
| macOS | 默认拒绝的 Seatbelt 配置，加 128 MiB 的 RSS 采样保护 |
| Go 监管层 | 父进程 1 秒截止，超时即杀进程并回收；每个 Runner 最多 8 个并发 worker，满了直接返回 `SCRIPT_BUSY`，不排队 |

其他平台无法安装沙箱时直接拒绝执行（fail closed）。

## 主要能力

- **两类脚本**：`FACTOR`（因子）从只读输入计算一个 `TEXT`、`NUMBER`（有限数）或 `BOOLEAN` 值；`RULE`（规则）返回布尔值，可以通过 `factors.name` 读取命名因子。
- **按需读取并缓存因子**：只有脚本真正访问到某个因子时，才回调你的 Go 函数去取值，同一次调用内最多取一次。`OnReuse` 会报告缓存命中，方便按顺序记录 EXECUTED/REUSED。
- **静态引用分析**：`Analyze` 不执行代码就能列出规则会读取的全部因子，保存规则前即可校验依赖。
- **三值逻辑辅助函数**：可视化规则编辑器可以生成封闭的 `__rule` 语法，取不到的事实记为"不可用"，而不是悄悄当成 `false`。`不可用 且 false` 为 false，`不可用 或 true` 为 true，最终结果为不可用时报错，绝不当成通过。
- **缺值即失败**：缺失的输入属性、未知因子、类型不符的因子值都会导致失败，脚本自己 catch 也没用。
- **确定性时间**：`context.now` 就是你传入的 Unix 毫秒数。
- **结构化错误**：错误码稳定；脚本错误带行号和列号。

## 快速开始

需要 Go 1.22+、C 编译器和 Python 3。Python 只在构建时用于校验 vendored 源码和嵌入 JS，构建过程不联网。

```sh
make worker      # 构建 bin/qjsbox-worker
make selftest    # 证明文件、网络、创建进程都被拒绝
make test        # go test -race，包含本平台的原生沙箱验证
make example     # 运行 examples/basic
```

把 `bin/qjsbox-worker` 和服务一起部署，将它的绝对路径传给 `qjsbox.New`。每个目标平台都要单独构建。worker 通过管道和 Go 通信，应由 Go 监管进程启动，不要把它的标准输出重定向到文件。

## 平台说明

- Linux 上的保证只有在 Linux 上跑 `make test` 才算验证过，macOS 通过不代表 Linux 沙箱生效。CI 两个平台都会跑。
- macOS 不允许非特权进程设置有限的 `RLIMIT_AS`/`RLIMIT_DATA`，所以整个进程的内存只靠 QuickJS 堆上限加 10 ms 一次的 RSS 采样来约束。**采样可能超出，不是操作系统级的硬上限。** 生产环境请用 Linux。

限制值、错误码、resolver 约定等详细说明见英文 [README](README.md)，原生协议和 vendored 源码来源见 [worker/README.md](worker/README.md)。

## 状态

qjsbox 是从一个内部规则配置系统中抽出来的小型库，在原系统里负责因子测试、单规则测试和场景模拟。目前还没到 1.0，Go API 可能调整。它不是完整的规则引擎，存储、版本管理和规则编排需要你自己实现。

## 许可证

Apache License 2.0，见 [LICENSE](LICENSE) 和 [NOTICE](NOTICE)。vendored 的 QuickJS 和 Acorn 采用 MIT 许可，见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。安全问题请见 [SECURITY.md](SECURITY.md)。
