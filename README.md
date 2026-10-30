# Magpie Model Guard

Magpie 插件：**模型级别**的降智检测与禁用。定时用可自动判分的探针题检测指定模型，检测未通过即拦截该模型的后续请求；支持自定义题目，适用于任意 `provider/model`。

- 探针经 Magpie 网关的 Chat Completions 接口发出，不绑定任何供应商或账号体系，**任意模型可用**。
- 检测题内置「糖果推理」；可添加自定义客观题（包含 / 精确 / 正则三种判分）。
- 每个目标独立的检测间隔与失败复检间隔；每日探针预算限制。
- Go 后台（单二进制，内嵌 Web 控制台）+ Magpie JS 插件宿主 + 网关门禁中间件。

## 工作流程

```text
客户端 -> Magpie 网关 -> model-guard 门禁中间件 -> 上游模型
                         |
                         +-- 未监测 / 检测通过 -------> 直接放行
                         +-- 探针别名请求 ------------> 剥掉前缀后放行（防自拦截）
                         +-- failed/review/unavailable -> 403 拦截，不发往上游

定时调度器 --到达检测时间--> 用选中题目探针 --> 判分
    pass  -> state=passed，按 interval 排下一轮
    fail  -> 同题独立复测一次，两次皆错 -> state=failed（禁用），按失败复检间隔复测
    error -> state=unavailable（保守禁用），冷却后重试；不计为答错
```

门禁文件由 Go 后台在每次状态变化时原子重写，Magpie 在一秒内热加载。探针请求使用
内部别名 `model-guard-probe/<alias>/<model>`，门禁剥掉前缀后放行，避免探针被自己拦截。

## 安装

### 从源码构建

需要 Go 1.26+：

```sh
go build -o bin/model-guard ./cmd/model-guard
magpie plugin add /absolute/path/to/magpie-model-guard
```

开发目录需包含 `plugin/`、`bin/model-guard`、`package.json`。跨平台构建：

```sh
sh scripts/build.sh
```

（产物：`bin/model-guard-<platform>-<arch>`，插件会按当前系统自动选择。）

### 首次配置

1. Magpie 插件入口 →「打开 Model Guard 控制台」。
2. 「连接设置」确认网关 URL 与 Key（本机默认 `http://127.0.0.1:3425` / `magpie`；插件会自动发现）。
3. 「监测目标」→ 刷新模型 → 选择完整模型 ID（如 `provider/model`）→ 添加监测。
4. 点「重测」立即跑一轮，确认链路正常。

## 检测规则

内置题库：

| ID | 题目 | 自动判分 | 规则 |
| --- | --- | --- | --- |
| `candy` | 糖果推理 | 是 | 回答中出现独立的 `21`（前后无数字）即通过 |
| `repeat` | 逐字复述 | 否 | 人工评价 |
| `japan` | 知识覆盖 / 日本首相 | 否 | 人工评价 |
| `person` | 知识覆盖 / 人物 | 否 | 人工评价 |
| `svg` | SVG 创作 | 否 | 人工评价 |

自定义题：「检测题库 → 新建题目」，判分类型：

- `contains`：回答包含期望关键字；
- `exact`：去除首尾空白后完全相等；
- `regex`：正则匹配（保存时校验）；
- 勾选「参与自动门禁」的题目才会影响模型状态。

一轮检测对目标的每道自动题各发一次请求；任一题失败会**独立复测一次**，两次皆错才判定
降智（`failed`，禁用）。网络错误、超时、HTTP 非 200、空回答、截断、预算耗尽均记为
`unavailable`：保守禁用但不计为答错，冷却（默认 15 分钟）后自动重试。

### 状态与操作

| 状态 | 含义 | 门禁 |
| --- | --- | --- |
| `unknown` | 从未检测 | 放行 |
| `checking` | 检测中 | 放行 |
| `passed` | 上轮通过 | 放行 |
| `failed` | 降智（连续两次答错） | **403 拦截** |
| `unavailable` | 检测不可用 | **403 拦截**（保守） |
| `review` | 人工暂停 | **403 拦截** |

控制台操作：重测（立即）、放行15分（临时放行）、暂停/恢复监测、删除目标。复检通过即自动
解除禁用；`unavailable` 冷却后自动复测。

### 预算与超时

- 每模型每日探针请求上限（默认 120，可设 1–10000）；
- 单次请求超时默认 180s，`max_tokens` 8192；
- 默认检测间隔 30 分钟，可按目标设置 5–10080 分钟；
- 失败复检间隔默认 60 分钟（可按目标设置）。

## 数据与安全

- 状态存于 `~/.config/magpie-model-guard/model-guard.db`（SQLite）；
- 管理 API 仅监听回环地址，Bearer token 鉴权，token 由插件入口注入控制台 URL；
- 探针只经过 Magpie 网关，不持有上游供应商凭据；插件不修改 Magpie 的供应商配置。

## 致谢

结构参考 [magpie-intelligence-check](https://github.com/mu-zi-lee/magpie-intelligence-check)
与 [magpie-codex-guard](https://github.com/Dsd1001/magpie-codex-guard)；糖果题文本与判分
规则来自社区通行版本。本项目与 Magpie 官方无关联。

## License

MIT
