# codemode-go

[![Go Reference](https://pkg.go.dev/badge/github.com/gtoxlili/codemode-go.svg)](https://pkg.go.dev/github.com/gtoxlili/codemode-go)
[![CI](https://github.com/gtoxlili/codemode-go/actions/workflows/ci.yml/badge.svg)](https://github.com/gtoxlili/codemode-go/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/gtoxlili/codemode-go)](https://goreportcard.com/report/github.com/gtoxlili/codemode-go)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**给 Go agent 的 code mode。** 模型写一段 JavaScript 程序来调你的工具,不必一轮只调一个。

[English](README.md)

![生产环境里的一次 run_code 调用。模型写的 JavaScript 程序先查看工作区,用 Promise.all 并行发起三路检索,再按命中结果分支做条件读取,接着调记忆工具、sleep、本地计算,最后返回一份紧凑报告。程序下方是这次运行的五条子调用时间线和各自耗时,再下面是工具结果:日志加上程序 return 的对象。](assets/code-call.png)

*八个步骤,三次检索并行跑完各约 130 毫秒,五次子调用,回来一份结果。程序中途读过的东西都留在了进程里。*

## code mode 是什么

给模型一个能跑程序的工具,把其余工具作为 API 暴露给这段程序。模型写 `await tools.search({query})`,不再是发一个 tool call 然后等一轮拿结果。

由此得到两件事。相互独立的调用真的同时跑,因为 `Promise.all` 是真并行,而"一次发两个 tool call 并指望运行时替你并行"不是。中间结果留在进程里,模型看到的是程序 return 的东西,不是它为了得出这个结论而翻过的 50 条命中。

这个做法有好几个名字。[Anthropic](https://www.anthropic.com/engineering/code-execution-with-mcp) 叫它 code execution with MCP,[Cloudflare](https://blog.cloudflare.com/code-mode/) 叫它 Code Mode,[CodeAct 论文](https://arxiv.org/abs/2402.01030)叫它 code-as-action。形状是同一个:把 JSON 工具调用换成一段程序。

这个库是它的 Go 实现,从一个已经在生产跑这套东西的 agent 里抽出来。

```js
// 一次工具调用,看 20 个候选人,只带回一份结果
const handles = await tools.list_candidates({campaign: "spring"});
const profiles = await Promise.all(handles.data.map(h => tools.fetch_profile({handle: h})));
const fit = profiles
  .filter(p => p.data.followers > 10000)
  .map(p => ({handle: p.data.handle, ratio: p.data.engagement / p.data.followers}))
  .sort((a, b) => b.ratio - a.ratio)
  .slice(0, 5);
return fit;
```

同样的活走直连调用是 21 次调用、至少 3 轮对话,而且 20 份原始资料全部进入上下文,并在这次会话剩下的时间里一直占着。

## 解决什么

**上下文。** 中间结果留在进程里。扫 50 条搜索命中只产生一条工具结果,下一轮不用再背着那 45 条最后没用上的。Anthropic 那篇文章里的一个案例是从 15 万 token 降到 2000。

**真的并行。** `Promise.all` 把调用放进并发池。等模型在下一轮再发一批 tool_call 做不到这件事。

**该用代码写的逻辑。** 筛选、打分、按 key 关联两个来源,程序里四行就写完了,换成模型推理则要先把数据全部贴进上下文,再用一整段文字描述它做了什么。

它的设计是和直连调用并存,不是取代它。多数轮次只有一两个工具调用,那种情况下走程序纯属额外开销,工具描述里写了什么时候值得写程序,模型据此自己选。

## 安装

```bash
go get github.com/gtoxlili/codemode-go
```

需要 Go 1.25 以上。核心模块只依赖 [goja](https://github.com/dop251/goja),不用 cgo,不用 Node,不用 Deno,不用容器。

## 用法

三步:绑定工具,挂载工具,教会模型。

```go
import "github.com/gtoxlili/codemode-go"

bindings := []codemode.Binding{{
    Name:   "search_files",
    Invoke: searchFiles, // func(ctx context.Context, argsJSON string) (string, error)
}, {
    Name:         "write_file",
    Invoke:       writeFile,
    Mutating:     true,
    ConflictKeys: func(args string) []string { return []string{"file:" + pathOf(args)} },
}}

tool := codemode.NewTool(codemode.Options{
    Bindings: bindings,
    Blocked: []codemode.Blocked{{
        Name:   "ask_user",
        Reason: "ask_user 会结束当前回合,程序里调不了,要问就在写程序之前问",
    }},
})
```

`tool` 提供 `Name()`、`Description()`、`Parameters()` 和 `Call(ctx, argsJSON)`,这是一个工具调用循环需要的全部。`Prompt` 返回配套的 system prompt 段:

```go
systemPrompt += "\n\n" + codemode.Prompt(codemode.PromptOptions{})
```

工具描述说的是这个工具是什么、程序能调哪些工具。prompt 段说的是怎么写程序:一次调用 resolve 出来的信封和直连调用拿到的一样,失败的调用 reject 成可以 catch 的错误,哪些调用是并行的,以及只有 return 值和 console 输出会回到对话。

### 被挡下的工具

`Blocked` 用来声明一个工具在模型的 tool list 里、但程序里调不了。这些名字会出现在自动生成的描述的排除名单中,程序里真去调会拿到你写的 `Reason` 作为该次调用的错误,而不是一句 unknown tool。

### 看到运行过程

`OnCall` 在每次子调用前后触发,`OnProgram` 每次运行触发一次,在程序开始之前:

```go
codemode.Options{
    Bindings: bindings,
    OnCall: func(ctx context.Context, ev codemode.CallEvent) {
        // ev.Seq, ev.Tool, ev.Args, ev.Phase, ev.Duration, ev.Err
        ui.Push(ctx, ev)
    },
    OnProgram: func(ctx context.Context, code, description string) {
        ui.Label(ctx, description) // "筛选 20 个候选人并打分"
    },
}
```

两个回调都在发起调用的那个 goroutine 上同步执行,回调慢会拖慢程序。事件带的是原始入参,不是摘要。

### 返回形状提示

工具协议只下发入参 schema,返回结构对模型是盲的,程序里写 `r.data.hits` 是在猜。`ReturnShape` 从 Go 返回类型推出一行紧凑提示,用在工具描述的末尾:

```go
desc += "\n\nReturns `{data: " + codemode.ReturnShape[SearchResult]() + "}`."
// Returns `{data: {query, hits: [{path, line: num, snippet}], truncated: bool, cursor?}}`.
```

裸字段名表示字符串,其他类型带标注,`?` 表示 omitempty。`PromptOptions` 的 `ReturnShapes` 设成 `true` 时,prompt 段会带上这套记法的说明;设成 `false` 时整段不提返回形状。

## 适配器

独立模块,核心包因此保持只有一个依赖。

### eino

```bash
go get github.com/gtoxlili/codemode-go/adapters/eino
```

```go
bindings, err := einocodemode.Bindings(ctx, myTools)
ct := codemode.NewTool(codemode.Options{Bindings: bindings})
myTools = append(myTools, einocodemode.NewTool(ct))
```

[eino](https://github.com/cloudwego/eino) 的工具没有"是否写入""碰哪个资源"这类信息,所以转出来的 binding 是 `Mutating` 为 false、`ConflictKeys` 为 nil,调度器把每个调用都当作无冲突。返回的是普通切片,交出去之前可以改。

### MCP

```bash
go get github.com/gtoxlili/codemode-go/adapters/mcp
```

```go
discovered, err := mcpcodemode.Tools(ctx, mcpClient)
ct := codemode.NewTool(codemode.Options{Bindings: mcpcodemode.Bindings(discovered)})
```

这种接法的前提是这些工具已经在模型的 tool list 里,60 个工具就是每轮 60 份 schema。另一种接法不把它们放进 tool list,改把目录写进程序工具的描述里,每轮成本变成一份描述:

```go
ct := codemode.NewTool(codemode.Options{
    Bindings:    mcpcodemode.Bindings(discovered),
    Description: intro + "\n\n" + mcpcodemode.Catalog(discovered),
})
```

后一种自带描述,因为自动生成的那句话说的是"程序能调你 tool list 里的工具",工具移出 tool list 之后这句不再成立。

配合 [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go) 客户端使用,stdio、SSE、streamable HTTP、进程内几种 transport 都可以。

## 横向对比

code mode 在几个生态里都有实现。差别在于模型写什么语言、由什么跑、以及周边机制要你自己补多少。

| 项目 | 模型写 | 跑在 | 宿主语言 | 隔离方式 |
|---|---|---|---|---|
| **codemode-go** | JavaScript | goja,进程内 | Go | 能力省略加采样护栏 |
| [Cloudflare Code Mode](https://blog.cloudflare.com/code-mode/) | TypeScript | Workers 上的 V8 isolate | TypeScript / Workers | V8 isolate,无网络 |
| [Anthropic code execution with MCP](https://www.anthropic.com/engineering/code-execution-with-mcp) | TypeScript | 自己选,它是一套做法而不是一个库 | 任意 | 自己建 |
| [UTCP code-mode](https://github.com/universal-tool-calling-protocol/code-mode) | TypeScript / Python | Node `vm` | TypeScript、Python | Node `vm` context |
| [Edison-Watch/mcp-code-mode](https://github.com/Edison-Watch/mcp-code-mode) | TypeScript | Deno | Python | Deno 权限加 AST 白名单 |
| [smolagents CodeAgent](https://github.com/huggingface/smolagents) | Python | 受限解释器,或 E2B / Docker | Python | 取决于你选的执行器 |
| [langchain-sandbox](https://github.com/langchain-ai/langchain-sandbox) | Python | Deno 里的 Pyodide | Python | Deno 权限 |
| [pydantic/mcp-run-python](https://github.com/pydantic/mcp-run-python) | Python | Deno 里的 Pyodide,以 MCP server 形式 | 任意 | Deno 权限 |
| [Protocol-Lattice/go-agent](https://github.com/Protocol-Lattice/go-agent) | Go | 它自己的 UTCP 运行时 | Go(限该框架) | 需要显式打开 unsafe tools |

**这个库强在哪。** 它是一个库,不是框架,也不是运行时。底下没有 Node、Deno、Docker 或者云平台,一个 Go 依赖、一个进程,agent 循环是手写的、eino 的还是 MCP 客户端,接法都一样。引擎之外盒子里还有:system prompt 段、会把碰同一个资源的调用串起来的调度器、写给模型自修正的失败分类、以及每次子调用前后的钩子。一次运行的启动开销是一个 goja VM,微秒级,所以只调两个工具的程序也不算亏。

**这个库弱在哪。** 它不是安全边界,细节见下,真需要边界的话上表里 Deno 和 V8 isolate 那几个确实更强。模型写的是 JavaScript 而不是 Go,Go 工具只能通过传进去的 binding 触达,没有办法把一个 Go 值直接交给程序。它也不生成带类型的 SDK,模型照着工具描述写代码,而不是照着有自动补全的 TypeScript 类型写,`ReturnShape` 缩小了这个差距但没有抹平。还有,程序和宿主共用一个进程的内存,所以内存那道护栏是带已知误差的触发线,不是硬顶。

## 程序能看到什么

| 名字 | 语义 |
|---|---|
| `tools` | `tools.name(args)` 返回 Promise,resolve 为该工具的结果,是 JSON 就解析好 |
| `ToolCallError` | 调用失败时 reject 的错误,带 `.toolName`,catch 之后可以继续 |
| `console` | log/info/warn/error/debug,都写进同一条日志通道 |
| `sleep(ms)` | 唯一的等待方式,`setTimeout` 那一家都不存在 |

加上 JavaScript 语言内建,没有别的。没有文件系统、网络、`require`、`process`、`fetch`,所有对外的效果都只能经过你传进去的绑定。

真正的边界是上面这份能力清单。剩下的部分说明白:**这不是安全沙箱。** 程序在进程内跑 goja,goja 没有 per-VM 内存上限,也没有指令计数钩子。墙钟、计算预算、内存行程靠 50 毫秒采样加上 goja 的 `Interrupt` 执行,能在一秒内掐掉热循环和内存爆涨,而这正是模型会误写出来的东西。挡不住的是一个能控制程序内容、并且刻意要绕过去的人。

## 资源护栏

| 护栏 | 默认值 | 说明 |
|---|---|---|
| 墙钟 | 10 分钟 | 先 Interrupt,5 秒宽限后硬停 |
| 计算预算 | 2 分钟 | 只计真正在跑 JS 的时间,等工具不算 |
| 内存 | 256 MB | 相对运行起点的堆增量,判死前强制 GC 复核一次 |
| 子结果累计 | 64 MB | 逐字节记账,不采样 |
| 调用深度 | 8192 | |
| 并发子调用 | 8 | `Promise.all` 拿到的并行度 |
| 单次运行子调用总数 | 200 | |
| console 输出 | 200KB / 2000 行 | 触顶即判失败,保留已收集的部分 |

都可以经 `Options.Limits` 覆盖。零值表示取默认值,这对只填一半的结构体是对的,对配置文件来的值则不是,一个写成 0 的 `wall_clock` 会静默变成 10 分钟,`Validate()` 就是用来报出这种值的。

计算预算和墙钟的区别在于计量口径。扇出 20 个慢接口可以跑好几分钟而不消耗计算预算,因为那些时间没有在跑 JavaScript。`while (true) {}` 则会在 2 分钟内耗尽它,远早于墙钟。工具慢和程序失控是两个问题,归两道护栏管。

## 失败

失败带分类,每一类对应模型的一种自修正路径。错误正文是写给模型读的,并附上程序崩溃前打印的尾部日志。

| 分类 | 怎么改 |
|---|---|
| `exception` | 修语法或逻辑 |
| `timeout` | 收窄循环,减少调用 |
| `compute-limit` | 少算一点,这一条和工具慢无关 |
| `memory-limit` | 别拼无界的字符串和数组 |
| `result-limit` | 在代码里筛选或聚合,不要全量持有 |
| `output-limit` | 少打印,改用 return |
| `invalid-return` | 返回可 JSON 序列化的数据 |
| `too-many-calls` | 不用改,这是有意止损 |
| `aborted` | 不用改,调用方取消了 |

单个子调用失败不会判死整次运行,它 reject 成程序可以 catch 的 `ToolCallError`,一个数据源挂掉不至于把已经做完的活全扔掉。没有 `await` 的调用照常执行,失败时以 `[unhandled rejection]` 日志行出现,不会静默消失。

## 调度

调用按程序里发起的顺序启动。普通调用进 `MaxParallel` 并发池。与在途调用共享 `ConflictKeys`、且两者至少一方会写的调用,先等池清空,独占运行。

key 由工具自己算,不由运行时算。运行时不认识任何工具的参数 schema,只有工具知道这次调用碰了什么。共享后端返回常量,资源 id 返回 `"deck:" + id`,文件返回绝对路径。key 是按字符串比的,所以 `out/a.jpg` 和 `./out/a.jpg` 是两个 key,它们之间的冲突看不见。

调用一结束就从冲突面里摘掉,因为两个调用只在重叠期间才谈得上冲突。不这样做的话,先写一份摘要再批量读回一批文件(包括刚写的那份),整个读扇出会被一个已经完成的写操作串起来。

## 常见问题

### 这是安全沙箱吗

不是。能力省略这一层是真的:程序没有文件系统、网络和 import,所有效果都要过传进去的 binding。资源护栏是另一回事,它是采样触发线,拦得住模型误写出来的东西,拦不住有意绕过的人。

### 为什么是 JavaScript 而不是 Go

因为模型写 JavaScript,而 goja 能在进程内跑它,不用 cgo,不用起子进程,不用容器。让模型写 Go 则意味着要有工具链、要有编译步骤,还要先把隔离做好才能跑第一行。模型写 JavaScript 的量也远大于写 Go,这直接反映在程序一次写对的比例上。

### 它会取代普通的工具调用吗

不会。它的设计就是和直连调用并存:多数轮次只有一两个调用,那种情况下走程序纯属额外开销,工具描述里写了什么时候值得写程序,模型据此自己选。

### 到底能省多少上下文

完全取决于你的工具返回多少数据、答案又需要其中多少。省下来的正是程序丢掉的那部分:扇出 50 条结果只 return 5 条,剩下 45 条从来没进过对话。Anthropic 那篇文章测的一个案例是 15 万 token 降到 2000。只取一样东西再原样返回的程序,一点也省不下来。

### 必须用 MCP 吗

不用。MCP 只是工具的一个来源,做适配器是因为那里收益最大。一个 `Binding` 就是一个名字加一个 `func(ctx, argsJSON) (string, error)`,Go 里能调的东西都可以是工具。

### 支持哪些 Go agent 框架

都支持,也都不绑定。核心产出的是名字、描述、JSON Schema 和一个 `Call`,接进你已有的循环即可。[eino](https://github.com/cloudwego/eino) 和 MCP 客户端有现成适配器。手写的 OpenAI 或 Anthropic SDK 循环大概五行接完。

### 需要装 Node、Deno 或 Docker 吗

不需要。goja 是纯 Go 写的 JavaScript 引擎,程序就在你的进程里跑。

### 模型写了死循环会怎样

大概两分钟后被计算预算掐掉,远早于墙钟。模型拿回一条 `compute-limit` 失败,内容是让它少算一点,并附上程序卡住之前打印的日志。`try/catch` 吞不掉这个中断,有测试钉着。

### 程序里能再调一次程序吗

不能。这个工具把自己排除在自己的绑定集之外,它的名字还是描述里排除名单的第一项,模型看得到。

## 延伸阅读

- [Code execution with MCP](https://www.anthropic.com/engineering/code-execution-with-mcp),Anthropic,把问题讲得最清楚的一篇
- [Code Mode: the better way to use MCP](https://blog.cloudflare.com/code-mode/),Cloudflare
- [Executable Code Actions Elicit Better LLM Agents](https://arxiv.org/abs/2402.01030),CodeAct 论文,量化数据的起点

## 状态

v0.x。引擎已经稳定并在生产使用,外围 API 可能还会调整。欢迎提 issue 和 PR。

MIT。
