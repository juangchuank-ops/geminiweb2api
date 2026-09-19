# GeminiWeb2API

把 [gemini.google.com](https://gemini.google.com/) 的 Web 端能力封装成 **OpenAI 兼容 API**，并配一套完整的**管理台 + 号池调度**。

前端界面参考 [grok2api](https://github.com/chenyme/grok2api) 的设计语言实现（React 19 + Vite + Tailwind 4，自建零依赖 shadcn 风格组件）。后端是纯 Go 标准库，无第三方依赖，单二进制 + 单 JSON 文件即可跑起来。

> **凭据就是一个 Cookie。** Gemini 的 Web 端没有签名、没有指纹、没有时间戳——会话完全由 Cookie 承载。这让加号简单到「复制粘贴」，代价是那个 Cookie 只有几小时寿命，于是**自动续期**成了这个项目最重要的功能，而不是附加项。详见「[Cookie 续期](#cookie-续期)」。

---

## 特性

**API 层**

- `POST /v1/chat/completions` — 支持流式（SSE）与非流式，兼容 OpenAI 请求/响应格式
- `POST /v1/images/generations` — 图像生成（返回 `url` 或 `b64_json`）
- `GET /v1/models` — 模型列表
- `GET /health` — 健康检查 + 号池概览

**号池管理**

- 多账号 Cookie 池，支持分组、优先级、单账号并发上限
- 两种账号类型：
  - **Cookie 账号** — 粘贴浏览器里的完整 Cookie，按账号自己的会话作答
  - **游客账号** — 不带任何凭据，用上游的匿名额度。额度小、能力弱，但整池 Cookie 都失效时还能兜底
- 四种调度策略：`least_inflight`（默认）/ `round_robin` / `priority` / `random`
- 粘性会话：同一会话（`user` 字段或 `X-Session-Id` 头）优先复用同一账号
- 失败自动降级：指数退避冷却（`cooldown`）、凭据失效标记（`invalid`）
- 请求级故障转移：单次请求内最多重试 `MaxAttempts` 个账号
- 批量导入（粘贴多行 Cookie）、导出、批量启停/改并发/清冷却/删号
- 后台异步探测连通性，不阻塞接口

**Cookie 自动续期**

- 后台按间隔（默认 10 分钟）轮换每个账号的 `__Secure-1PSIDTS`，账号之间串行留间隔，避免同 IP 突发被风控
- 轮换结果（成功 / 失败 / 限流 / 跳过 / 失效、连续失败次数、上次尝试时间）直接落在账号记录里，号池列表有一列 **Cookie 刷新**
- 单账号可「立即刷新」，也可一键「刷新全部」
- **限流不计入失败**——限流是调用方太频繁，账号本身没问题，把它算成失败会让一个繁忙的网关慢慢删掉自己的号池
- 连续失败达到阈值才退役，且**只有 401 类才算数**——这是给被吊销的会话准备的安全阀，不是给网络抖动用的
- 失败也写「上次刷新时间」，避免上游不可达时每个 tick 重试一次，把一次故障变成请求风暴

**连通性探测与「静默降级」检测**

这是本项目最容易被忽略、也最值钱的一点：

> `__Secure-1PSIDTS` 过期后，请求**不会失败**。上游照常回 200、照常给答案，只是按**游客额度**作答——额度小得多，模型档位也不是你要求的那个。响应里没有任何一个字段说「你的 Cookie 过期了」。

所以探测分两个问题、两个字段回答：

| 字段 | 含义 |
| --- | --- |
| `available` | 应用外壳加载成功（任何人都能成功） |
| `authenticated` | 回的是**真实会话**，不是游客 |

`available && !authenticated` 就是「Cookie 已失效，但一切看起来正常」——号池列表会用琥珀色标出来。没有这个区分，一个整池失效的网关会一直显示健康，直到你发现答案怎么变差了。

**管理台**

- 仪表盘：调用量趋势、模型分布、账号排行、资源占用
- 号池管理、客户端密钥、模型目录、生成画廊、请求审计、系统设置
- 中英双语，明暗双主题
- 管理端会话基于 Bearer Token，密码用 HMAC-SHA256 迭代 12 万次加盐存储

**审计**

- 每次网关请求落一条记录（模型、账号、状态码、耗时、token 数）
- 可配置保留天数与最大条数，后台 janitor 定时清理
- 请求体记录可选、可限长

---

## 快速开始

### 环境要求

- Go 1.24+
- Node.js 20+（只在需要重新构建前端时用到）
- **能访问 `gemini.google.com` 和 `accounts.google.com` 的出站网络**。所在地区不提供 Gemini 时，必须给上游配代理（`upstream.proxy`），否则请求会拿到 `1060` 地区受限

### 构建

```bash
# 1. 前端（产物输出到 frontend/dist）
cd frontend
npm install
npm run build

# 2. 后端（产物是仓库根目录的 geminiweb2api 可执行文件）
cd ../backend
go build -o ../geminiweb2api ./cmd/geminiweb2api
```

### 运行

```bash
# 指定初始管理员密码（首次启动写入，之后改密走管理台）
GEMINIWEB2API_ADMIN_PASSWORD=你的密码 ./geminiweb2api -addr 127.0.0.1:8080
```

打开 `http://127.0.0.1:8080`，用 `admin` / 你设置的密码登录。

> 不传 `GEMINIWEB2API_ADMIN_PASSWORD` 时，首次启动会随机生成一个密码并打印在控制台。

### 加号

**取 Cookie**：登录 [gemini.google.com](https://gemini.google.com/)，F12 打开开发者工具 → `Application` → `Cookies` → `https://gemini.google.com`，把 **`__Secure-1PSID`** 和 **`__Secure-1PSIDTS`** 这两条复制出来，拼成一段：

```
__Secure-1PSID=g.a000…; __Secure-1PSIDTS=…
```

更省事的做法：在 `Network` 面板随便点一个请求，把请求头里 `Cookie:` 后面那**一整段**复制下来直接粘——解析是宽容的，多出来的 Cookie 会被原样带着走（Google 的边界检查的是整个 Cookie jar，而不只是这两条）。

**录入**：管理台 → **号池管理** → 添加账号 → 类型选 `Cookie`，把这一整段粘进「Cookie 凭证」即可。

保存后系统会在后台探测一次连通性，不卡界面。

批量加号时，导入框每行一个，支持这两种写法：

```
__Secure-1PSID=g.a000…; __Secure-1PSIDTS=…
主号----__Secure-1PSID=g.a000…; __Secure-1PSIDTS=…
```

> 已经存在的账号会被**更新**而不是报错——Cookie 过期后重新导入同一行就能刷新，这是批量换 Cookie 的正路。

**游客账号**：类型选 `游客`，不填任何凭据即可。它是兜底，不是主力。

### 调用

```bash
# 先在管理台「客户端密钥」建一个 key
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gm-xxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.8-flash",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": true
  }'
```

---

## 模型映射

模型目录（`internal/gemini/models.go` 的 `builtinModels`）**以用户看得见的营销名为主键**，管理台的模型列表直接从它生成，不存在两份列表漂移的可能。

| 模型 ID | 上游 mode | hex id | 说明 |
| --- | --- | --- | --- |
| `gemini-3.8-flash` | 1 | `56fdd199312815e2` | **默认模型**。网页端当前的 Flash，速度与质量均衡 |
| `gemini-3.8-flash-thinking` | 2 | 不发头 | 3.8 Flash 思考模式，输出最长（约 2 万字），适合难题 |
| `gemini-3.1-pro` | 3 | `e6fa609c3fa255c0` | 3.1 Pro，需要账号有 Pro/Ultra 订阅 |
| `gemini-auto` | 4 | 不发头 | 由上游按问题难度自动选档 |
| `gemini-3.8-flash-thinking-lite` | 5 | 不发头 | 3.8 Flash 动态思考，按需决定思考深度 |
| `gemini-3.5-flash-lite` | 6 | `cf41b0e0dd7d53e5` | 最快最省；免费号被降级时落到的就是它 |
| `gemini-3.6-flash` | 1 | `fbb127bbb056c959` | 上一代 Flash，留着是因为旧客户端会写死这个名字 |

**为什么主键必须是营销名。** 2api 面对的是「模型」那一栏由人照着模型列表填进去的客户端。名字如果写成 `gemini-flash` 这种自造的档位描述，`/v1/models` 就会列出一堆没人会去请求的字符串——对读它的人来说和空目录没有区别。所以目录里挂的是 `gemini-3.8-flash` 这种官方名；而这个项目早期用过的名字（`gemini-flash`、`gemini-flash-plus`…）全部降级成 `aliasModels` 里的别名，仍然可用。

**hex id 认的是模型，不是档位。** 这一点曾经搞反过：`56fdd199312815e2`（3.7/3.8 Flash）被当成某个模型的「Plus」「Advanced」变体列了三遍，而模型自己那一项挂的 hex 是 `fbb127bbb056c959`——那是 **3.6** Flash。结果就是**默认模型指向上一代**，当前那一代藏在一个人人都会以为要付费才能用的名字后面。档位后缀是虚构的，那几项其实是更新的模型被归错了档。

**同一个 hex 可以出现在两个 mode 下**（3.8 Flash 和它的思考模式），但**不能只靠 capacity 区分**——那正是上面那个错误的形状。`models_test.go` 里两条测试分别钉住这两点。

**Google 会原地升级模型。** 3.7 Flash 变成 3.8 Flash 时 hex 没变，所以 hex 不等于版本号；`gemini-3.7-flash` 因此是别名而不是目录里的第二项。

别名：`gemini-2.5-pro`、`gpt-4o`、`deepseek-reasoner` 之类的常见写法都有映射，见 `aliasModels`。客户端指定了目录里没有的模型名时，会回落到 `upstream.defaultModel`（默认 `gemini-3.8-flash`）——直接拒绝会让所有硬编码模型名的客户端一起报错，而它们大多数都这么干。

> **思考深度可以不换模型就调。** 请求里带 `reasoning_effort`：`none` / `minimal` / `low` 表示用模型自带的档位，其余任何取值（`medium`、`high`……）都强制最深的思考模式。
>
> 推理内容会被自动分离到 `reasoning_content`，不会混进正文——这是按 payload 字段分流的，不依赖上游的事件编号。

hex id 的归属来自两个独立逆向实现的交叉验证（[yeahhe365/Gemini-Nexus](https://github.com/yeahhe365/gemini-nexus) 的模型目录变更记录、[zexadev/gemini-web2api-go](https://github.com/zexadev/gemini-web2api-go) 的版本历史，以及 [zhiyu1998/Gemi2Api-Server](https://github.com/zhiyu1998/Gemi2Api-Server) 里原样透传的抓包请求头）。这些 id 是不透明的，Google 会轮换——所以它们是**数据**，轮换时改配置而不是改代码。

---

## 配置

### 启动参数 / 环境变量

| 参数 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `-addr` | `GEMINIWEB2API_ADDR` | `127.0.0.1:8080` | 监听地址 |
| `-data` | `GEMINIWEB2API_DATA` | `data` | 数据目录 |
| `-static` | `GEMINIWEB2API_STATIC` | `frontend/dist` | 前端产物目录 |
| `-admin-user` | `GEMINIWEB2API_ADMIN_USER` | `admin` | 初始管理员用户名 |
| `-admin-password` | `GEMINIWEB2API_ADMIN_PASSWORD` | 随机 | 初始管理员密码 |

### 运行时设置

以下配置存在 `data/app.json` 里，可在管理台「系统设置」直接改，改完即时生效：

- **服务**：最大并发请求数、管理员用户名
- **上游**：上游地址、语言、兜底模型、请求超时、流空闲超时、**Cookie 续期接口**、代理、User-Agent
- **路由**：调度策略、冷却基数/上限、最大重试次数、容量等待、粘性会话 TTL、是否优先空闲账号
- **审计**：保留天数、最大记录数、是否记录请求体、请求体长度上限
- **媒体**：生成文件目录、公开访问前缀、总容量上限、是否自动转存
- **Cookie 续期**：开关、轮换间隔、账号间隔、单次超时、连续失败退役阈值

---

## 上游协议

Gemini 没有公开 API，这里是把它 Web 端的调用方式复刻出来。协议来自对网页端行为的抓包与两个开源逆向实现的交叉验证（[HanaokaYuzu/Gemini-API](https://github.com/HanaokaYuzu/Gemini-API)、[Sophomoresty/gemini-web2api](https://github.com/Sophomoresty/gemini-web2api)）。

### 凭据：只有 Cookie

```
__Secure-1PSID     长寿命，账号的真正身份
__Secure-1PSIDTS   只有几小时，需要轮换（见「Cookie 续期」）
```

没有签名、没有指纹、没有时间戳。整段 Cookie 被**原样保存和回放**，因为 Google 的边界检查的是整个 jar，而不是这两个名字——从两条 Cookie 重新拼一个 jar 出来，那已经是另一个 jar 了。

> 解析是**刻意宽容**的：`Cookie:` 前缀、换行、多余空白、无关 Cookie 都接受。这里严格一点的失败模式是「账号看起来无效，而操作员看不出为什么」。

### 初始化：从 `/app` 里抠三个值

```
GET {base}/app          （多账号时 {base}/u/{N}/app，N 就是账号序号 authUser）
```

从 HTML 里正则提取：

| 名字 | 用途 |
| --- | --- |
| `SNlM0e` | `at`，CSRF 令牌，生成请求必带 |
| `cfb2h` | `bl`，build label，放在 query 里 |
| `FdrFJe` | `f.sid`，会话 id，放在 query 里 |

顺带记下 `TuX5cc`（语言）和 `qKIAYe`（push id）。

> **`bl` 会过期。** 过期时生成请求返回 **HTTP 405**，所以适配器对 405 会重拉一次 `/app` 再重试——而不是把它当成错误报出去。

### 生成

```
POST {base}/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate
     ?bl={bl}&hl={hl}&_reqid={n}&rt=c&f.sid={f.sid}
Content-Type: application/x-www-form-urlencoded

at={at}&f.req={urlencode(JSON.stringify(payload))}
```

`payload` 是一个**稀疏的 81 元素数组**，内层还要再 `JSON.stringify` 一次（所以是「双层编码」）。用到的槽位：

| 槽位 | 内容 |
| --- | --- |
| `0` | `[prompt, 0, null, attachments, null, null, 0]` |
| `1` | 语言，如 `["en"]` |
| `2` | 会话元数据：首轮用默认值，续轮填上次拿到的会话 id |
| `6` | `[1]` |
| `7` | `1`（流式） |
| `10` | `10` |
| `17` | `[[thinkMode]]`，**思考深度** |
| `19` | Gem id（应用了系统提示词时） |
| `30` | `[4]`，输出上限 |
| `41` / `45` | 持久化开关 |
| `59` | 每个请求新生成的 UUID |
| `61` | `[]` |
| `68` | `1` |
| `79` | **模型 mode 编号** |
| `80` | `1` |

**mode 编号**（枚举来自实证，不是列表下标）：

| 编号 | 含义 |
| --- | --- |
| 1 | FAST |
| 2 | THINKING |
| 3 | PRO |
| 4 | AUTO |
| 5 | FAST_DYNAMIC_THINKING |
| 6 | FLASH_LITE |

**思考深度**槽位（`17`）只有两个实测值：普通 mode 用 `4`，思考 mode 用 `0`。

### 模型选择要两处一致

选了模型之后，**编号和请求头必须说的是同一件事**，否则上游会拒绝：

```
x-goog-ext-525001261-jspb: [1,null,null,null,"<hex>",null,null,0,[4,5,6,8],null,null,<capacity>,null,null,<mode 编号>]
```

- `<hex>` 是该模型的内部 id（如 `gemini-3.8-flash` 是 `56fdd199312815e2`）
- `<capacity>` 是模型在网页端自带的档位值：`1` 是免费档被服务的那些（3.6 Flash、3.5 Flash-Lite），`2` 是需要订阅的那些（3.8 Flash、3.1 Pro）。**它是模型的属性，不是调用方能表达的选择**，所以目录里每个模型只有一个值，没有「同一模型 × 三档」的排列
- `<mode 编号>` 必须和 payload 槽位 `79` 一致

对不上时的报错：

| 代码 | 含义 |
| --- | --- |
| `1050` | 编号和请求头互相矛盾 |
| `1052` | 模型头声明与 payload 对不上，或账号的订阅撑不起所选模型 |

> **`gemini-3.8-flash-thinking` / `gemini-auto` / `gemini-3.8-flash-thinking-lite` 没有 hex id**，所以走的是退化路径：**不发模型头，只按 mode 编号路由**。这是刻意的——猜一个 id 只会换来 `1052`，而不发头能让这些模式真的可用。思考模式尤其如此：有实现声称它和 3.8 Flash 共用同一个 hex，但那条路没有抓包佐证，而不发头是验证过能用的。

### 流式解析

响应是一段 `)]}'` 反 CSRF 前缀 + **长度前缀分帧**：

```
)]}'

123
[["wrb.fr","<rpcid>","<JSON 字符串>"]]
```

长度前缀是 `<数字>\n<载荷>` 重复。**这个前缀不能全信**——见下。

内层 JSON 的取值路径：

| 路径 | 内容 |
| --- | --- |
| `[1]` | 会话元数据（会话 id / 回复 id，续轮要用） |
| `[4]` | 候选列表 |
| `candidate[1][0]` | **累积正文**（注意是累积的，要自己算增量） |
| `candidate[37][0][0]` | 思考内容 |
| `candidate[8][0] == 2` | 这一轮结束 |

#### 长度前缀只当提示，不当事实

这是本项目踩过最隐蔽的一个坑。

最初解析器按「UTF-16 code unit」推进，并且有一条测试专门钉住这个假设。问题是**那条测试的 fixture 是用被测的同一个函数生成的**（`buildStream` 里写 `UTF16Len(payload)`），所以它只能证明解析器和自己的假设自洽，永远无法证伪单位到底是什么。真实抓包一进来就露馅：声明长度不落在帧边界上，解析器第一帧就错位，之后**再也找不到任何一帧**——表现为「空回答」，不是报错。

有个纯算术事实能把范围收窄：对任意 UTF-8 字符串，`UTF16Len(s) ≤ len(s)` 恒成立（BMP 字符 ≥1 字节记 1 单位；astral 字符 4 字节记 2 单位）。所以按单位推进**只会落短，不可能越过载荷尾部**。既然观察到的是「多算」，那它计的就**不是** UTF-16 单位——多半把某个分隔符也算进去了。

与其去猜单位，不如不信它。两个独立实现早就得出了同一结论：

- `tmc/nlm`：*"the exact counting varies. Instead of trusting the length values, extract JSON arrays directly by finding balanced brackets"*
- `zexadev/gemini-web2api-go`：逐行扫，`strings.Contains(line, "wrb.fr")` 之后直接 `json.Unmarshal`，**完全没有长度前缀算术**

所以这里的做法是：**长度前缀当快路，边界对不上就按载荷自身的括号配平重新切**。

1. 读出声明长度，算出候选边界
2. 校验边界：其后必须是「空白 + 数字 + 换行」（下一帧的长度标记）。**空剩余算校验失败**——流中途「什么都没有」分不清是「帧正好结束」还是「下一个字节还没到」，把它当成合法边界正是让偏小的声明长度截掉帧尾、静默丢帧的原因
3. 校验不过 → 从载荷开头的 `[` 做括号配平（会跳过字符串字面量与转义，所以代码和正文里的 `]` 不会提前结束帧）重新定位
4. 结构也找不到 → **等更多字节**，绝不按一个刚校验失败的声明长度硬切。这一点是修这个 bug 时自己踩进去的：第一版兜底会在帧还差最后一个字节时「按长度完成」它，等于把帧整个吃掉

`FrameParser.Resyncs()` 会统计有多少帧走了结构化兜底。非零不是错误，但**每一帧都在重同步**就说明上游又改计数方式了，快路已经名存实亡。
| `candidate[12]` | 富内容（媒体，图片在这里） |

### 错误码

错误以 `BardErrorInfo [code]` 的形式嵌在一个**看起来成功**的响应里，所以必须专门识别：

| 代码 | 含义 | 处理 |
| --- | --- | --- |
| `1013` | 暂时性错误 | 可重试 |
| `1016` | 未认证 | Cookie 已不是会话 → `invalid` |
| `1037` | 配额用尽 | 冷却 |
| `1050` | 编号与请求头不一致 | 配置错误 |
| `1052` | 模型头与 payload 对不上，或账号订阅撑不起所选模型 | 换模型或换号 |
| `1060` | 地区受限 | 需要代理 |

另有 HTTP `405` = build label 过期（重拉 `/app` 后重试）。

---

## Cookie 续期

这是整个项目里最重要的机制，值得单独说清楚为什么。

### 问题：失效是静默的

`__Secure-1PSIDTS` 只有几小时寿命。它一过期，上游**不会报错**：

```
HTTP 200 ✓   答案正常返回 ✓   状态码没有任何异常 ✓
```

变的只有两件事：额度小得多，模型档位可能不是你要求的那个。**响应里没有任何字段说「你的 Cookie 过期了」。**

也就是说，一个「每个请求都成功」的网关，可能已经整池按匿名游客在作答，而管理台显示一切健康。

### 机制

```
POST https://accounts.google.com/RotateCookies
Content-Type: application/json
Cookie: {完整 Cookie jar}

[000,"-0000000000000000000"]
```

请求体看着像占位符，它**就是**占位符：这个端点不接受有意义的输入，完全靠 Cookie jar 认证，然后在 `Set-Cookie` 里回一个新的 `__Secure-1PSIDTS`。

- 返回的 jar 是**完整更新后的 jar**，整份都会被持久化——不只是 PSIDTS，响应还会重发其它 Cookie，下一次请求要带着
- 调用过快会拿到 **429**，而且之后一段时间持续限流。参考实现定的间隔是 10 分钟，这也是默认值
- **401 / 403** 意味着 `__Secure-1PSID` 本身没了，账号已死，重试无用

### 几个刻意的设计选择

**限流不算失败。** 429 说的是调用方太频繁，不是账号有问题。把它算进退役计数，一个繁忙的网关会慢慢删掉自己健康的号池。所以限流记 `throttled`，用琥珀色而不是红色显示，也不增加失败计数。

**失败也写「上次刷新时间」。** `RefreshAt` 同时充当「还没到点」的标记。只记成功的话，上游不可达时每个 tick 都会重试一次，把一次故障变成针对「最怕请求风暴的那个端点」的请求风暴。

**只有 401 类才计入退役。** 一次 401 也可能是短暂的 consent 跳转，所以达到阈值（默认 3 次）才标记失效。阈值是给被吊销的会话准备的安全阀，不是给网络抖动用的。

**退役用 `invalid` 而不是 `disabled`。** 这两个状态含义不同：`disabled` 是「操作员把它关了」，`invalid` 是「这个凭据死了」。只有后者应该提示重新粘贴 Cookie。

**账号之间串行留间隔。** 轮换是风控敏感调用，同一个 IP 突发是让整个网段被限流的典型特征。默认 `gapSeconds = 3`。

**续期接口是可配置的。** 默认 `https://accounts.google.com/RotateCookies`，但它和主站**不同域**——所以走镜像部署时必须能把续期指到同一个地方。另外，一个硬编码的端点是一个任何测试都碰不到的端点，这也是它被提成设置项的原因之一。

---

## 架构

```
                    ┌──────────────────────────────┐
   OpenAI 客户端 ──▶│  gateway   /v1/*             │
                    │  · 鉴权（客户端密钥）         │
                    │  · 限流（RPM / 并发）         │
                    │  · SSE 转发                  │
                    └──────────┬───────────────────┘
                               │ Acquire / Release
                    ┌──────────▼───────────────────┐
                    │  pool   号池调度              │
                    │  · 策略选择 / 粘性会话        │
                    │  · 冷却退避 / 故障转移        │
                    └──────────┬───────────────────┘
                               │
      管理台 ──▶┌──────────────▼───────────────────┐
                │  admin   /admin/api/*            │
                │  · 账号 / 密钥 / 模型 / 审计      │
                │  · 连通性探测（可用 vs 已认证）   │
                └──────────┬───────────────────────┘
                           │
                ┌──────────▼───────────────────────┐
                │  store   内存态 + 单文件持久化    │
                │  · 读走 RWMutex，写走后台单写者   │
                │  · Settings 走 atomic 快照        │
                └──────────┬───────────────────────┘
                           │
   定时轮换 ──▶┌───────────▼──────────────────────┐
               │  refresher  Cookie 续期调度       │
               │  · 按账号自己的间隔判断到期       │
               │  · 限流不计数、失败也记时间戳     │
               └───────────┬──────────────────────┘
                           │
                ┌──────────▼───────────────────────┐
                │  gemini  上游客户端              │
                │  · /app 初始化（at / bl / f.sid）│
                │  · StreamGenerate + 分帧解析     │
                │  · RotateCookies                 │
                └──────────────────────────────────┘
```

### 目录

```
backend/
  cmd/geminiweb2api/      入口：路由装配、静态托管、优雅关闭
  internal/config/      启动参数 + 运行时设置模型（含旧默认值迁移）
  internal/store/       状态与持久化（含回归测试）
  internal/pool/        号池调度
  internal/gemini/      上游协议客户端
                        client.go    初始化 / 生成 / 媒体
                        payload.go   81 槽位 payload 构造
                        frame.go     长度前缀分帧 + 括号配平兜底
                        models.go    模型目录 + 模型请求头
                        cookie.go    Cookie jar（保序解析与回放）
                        rotate.go    RotateCookies
                        probe.go     连通性探测（可用 vs 已认证）
                        session.go   会话与重定向校验
                        errors.go    BardErrorInfo 分类
  internal/refresher/   Cookie 续期调度（不含 HTTP，客户端注入）
  internal/gateway/     OpenAI 兼容层 + 限流
  internal/admin/       管理台 API
frontend/
  src/app/              壳层与路由
  src/components/ui/    自建 UI 组件（零依赖）
  src/features/         各功能页
  src/shared/           API 客户端、鉴权、i18n、工具
tools/
  smoke.py              端到端冒烟测试
  contract.py           前后端接口契约检查（路径 / 方法）
  fields.py             DTO 字段契约检查（响应字段 / TS 类型）
  render.mjs            真实浏览器渲染 + 控制台流程检查（CDP，需本机 Chrome）
  secret_scan.py        工作树 + 全历史密钥扫描（推送前跑）
  hooks/pre-commit      提交前自动跑 secret_scan.py --tree-only
```

### 持久化设计

`data/app.json` 是唯一的状态文件。所有变更先改内存，再由**单个后台协程**防抖（40ms）后原子落盘（写临时文件 + rename）。

请求处理路径**不会**在持锁期间做磁盘 I/O，因此慢速或被占用的文件系统不会拖垮服务。运行时设置额外维护一份 `atomic.Value` 快照，使得已经持有写锁的回调（例如账号探测结果回写）也能安全读取配置——Go 的 `sync.RWMutex` 不可重入，这一点是硬性要求。

> ⚠️ **默认值是「安装时冻结」的，这一点会影响升级。**
> 全新安装会把整套默认设置写进 `app.json`，而 `Normalize` 只补**缺失**的值、不动**已存在**的值。
> 所以**改一个默认值只会影响到新安装**：存量实例的文件里躺着旧值，且它看起来完全正常，没有任何症状。
> 一个坏默认值就这样活过了一次「修好默认值」的发布。
>
> 处理办法是**迁移**：`Normalize` 里维护一张已知坏值的清单，命中就改回默认。
> 目前有一条：`upstream.baseURL` 曾经是 `https://gemini.google.com/app`（带 `/app` 后缀，会让 `/app` 初始化路径拼成 `/app/app`），命中就改回 `https://gemini.google.com`。
>
> 迁移结果会**写回磁盘**，不留「文件与实际生效配置不一致」的状态。
> 以后再加默认值修复，记得同时加进这张清单——否则修了等于没修。

---

## 测试

```bash
cd backend
go test ./...            # 单元测试（含并发/重入锁回归）
go vet ./...
```

覆盖七个包，其中六个完全不依赖网络：

| 包 | 覆盖内容 |
| --- | --- |
| `internal/store` | 配置快照的并发读写、写锁内重入读配置（死锁回归）、快照隔离、**从 Cookie 自动派生账号身份**、重复 PSID 拒绝、**响应里永不出现原始 Cookie**、内置模型目录与客户端目录一致 |
| `internal/pool` | 账号筛选（禁用/失效/冷却过期）、**Cookie 账号才要求 PSID、游客不要求**、PSID 被清空后不可调度、四种调度策略、粘性会话、退避与封顶 |
| `internal/gemini` | payload 槽位构造与思考深度、模型请求头的两处一致性、**长度前缀分帧 + 括号配平兜底**（声明长度偏大/偏小/按字节计都能恢复，含跨 chunk 断裂）、`)]}'` 前缀处理、`BardErrorInfo` 分类（含解码后 payload 里的 `["BardErrorInfo",[1037]]` 形式）、Cookie jar 的保序解析、模型目录完整性 |
| `internal/refresher` | 到期判定、**失败也写 `RefreshAt`**、**限流不计入退役**、401 达阈值才退役、成功清零失败计数、游客跳过而非失败、单账号刷新、**同一时刻只跑一次扫描**、扫描按间隔串行 |
| `internal/admin` | 从粘贴里读出凭据、标签不是凭据、拒绝不可用的 Cookie、换 Cookie 会重置续期记录、导入的各种形状、设置接口逐字段与 struct 的 json tag 比对、**刷新路由的状态码契约**（被拒必须 502 且带原因、限流必须是 200）、**游客探测不会把自己弄退役** |
| `internal/config` | 旧默认值迁移、迁移不误伤刻意的覆盖值 |
| `internal/gateway` | 端到端请求路径——鉴权、限流、故障转移、OpenAI 响应格式、流式、**累积文本不重复**、思考模式不发模型头、`reasoning_effort` 落到 payload、配额用尽触发冷却、build label 过期重试一次、审计、图像 |

> `internal/gateway` 的测试用 `httptest` 顶替上游，并且桩说的是**真实的 batchexecute 协议**（真的 `)]}'` 前缀、真的长度前缀分帧、真的 `BardErrorInfo` 帧），因此不需要真实 Cookie 就能覆盖完整链路。

端到端：

```bash
# 先启动服务，然后：
python tools/smoke.py --base http://127.0.0.1:8080 --password 你的密码 --skip-upstream
```

`--skip-upstream` 会跳过真正打上游的用例（没有有效 Cookie 时会一直等到超时），其余约 60 项断言覆盖管理台、网关错误路径与号池行为。它还会专门验几件容易出错的事：

- 原始 Cookie **永不**出现在响应里
- 游客账号能被建出来，而「选了 Cookie 类型却不粘 Cookie」必须被拒
- 刷新概览的 `counts.pending` **不包含**不可轮换的账号（游客、已禁用）
- 没有真实会话时刷新**不会被报成成功**，且 502 里带着原因

接口契约：

```bash
python tools/contract.py --base http://127.0.0.1:8080 --password 你的密码
```

前端是编译产物，路由写错只会在浏览器里变成 404。这个脚本把控制台**实际会发的每一个请求**都重放一遍，只有 404/405 才算失败（400 是参数校验、401 是鉴权，都说明路由命中了）。改完任一侧的接口路径后跑一下，能立刻发现前后端对不上的地方。

> 它会真的触发一次轮换扫描（`/admin/api/refresh/run`），也会建号删号。对着一次性实例跑，别对着正在服务的生产实例跑。

字段契约：

```bash
python tools/fields.py --base http://127.0.0.1:8080 --password 你的密码 --src frontend/src
```

路由对了不代表字段对得上——后端漏掉或改了一个字段名，页面只会静默显示空白。这个脚本从前端 api 层的 `export type Xxx = {...}` 解析出每个 DTO 期望的字段，再拿真实响应逐字段核对。集合为空时自动降级为「前端 DTO vs Go struct 的 json tag」静态比对，保证覆盖率不打折。

嵌套对象会被展开成点路径（`resources.routableAccounts`）逐个核对，而不是只看第一层。判定用的是「路径存在性」而非「值非空」，因为 `quota: AccountQuota | null` 这类字段本来就允许是 `null`。

脚本每次运行都会先自检路径判定函数本身，避免出现「永远返回通过」的假绿。

真实渲染（需要本机有 Chrome）：

```bash
# 先起一个带调试端口的 headless Chrome
# macOS / Linux：
chrome --headless=new --disable-gpu --no-first-run --no-default-browser-check \
       --remote-allow-origins=* --remote-debugging-port=9222 \
       --user-data-dir=/tmp/chrome-gemini about:blank

# Windows（Git Bash）——--user-data-dir 必须用 Windows 路径，见下方说明：
"/c/Program Files/Google/Chrome/Application/chrome.exe" --headless=new --disable-gpu \
       --no-first-run --no-default-browser-check --remote-allow-origins=* \
       --remote-debugging-port=9222 \
       --user-data-dir="C:\Users\你的用户名\AppData\Local\Temp\chrome-gemini" about:blank

node tools/render.mjs http://127.0.0.1:8080 http://127.0.0.1:9222 你的密码
```

> **Windows 上 `--user-data-dir` 必须写 Windows 路径**（如 `"C:\Users\你\AppData\Local\Temp\chrome-gemini"`）。传 `/tmp/...` 时 Chrome 会照常启动、日志一片空白，但**调试端口永远不会监听**，`curl http://127.0.0.1:9222/json/version` 一直连接被拒——看起来像端口被占，实际是路径没被认。另外本地回环请求记得加 `--noproxy '*'`，否则环境里的 `http_proxy` 会把它塞给代理，同样是连不上的假象。

前三个脚本都是 HTTP 层面的，看不见 React 渲染崩溃、未捕获的 Promise 异常或者一片空白。这个脚本通过 CDP 驱动真实 Chrome，逐页走一遍控制台，检查：有没有抛异常 / 有没有落到错误边界 / `#root` 是否为空 / 页面标题对不对。顺带验证鉴权守卫——已登录访问 `/login` 必须被弹回仪表盘。

跑完记得收尾，否则会在后台留一堆进程和磁盘垃圾：

```bash
taskkill //F //IM chrome.exe //T     # Git Bash 里参数要写双斜杠
```

> 如果渲染检查报 `SecurityError: Failed to read the 'localStorage' property`，那是**目标站不可达**——导航失败后页面停在 `about:blank`，而它的 origin 是 opaque 的，读写 localStorage 一律被拒。先去 `curl <base>/health` 确认服务起来了，别去改 localStorage 相关代码。

### 推送前：密钥扫描

```bash
python tools/secret_scan.py              # 工作树 + 全部历史
python tools/secret_scan.py --tree-only  # 只扫工作树，够快，可以放进 pre-commit
```

这个仓库的历史上曾经躺着**一个可用的 Gemini JWT**（旧 Python 版的 `config.json`，已清除）。所以扫描器**默认走完整历史**，而不只是当前工作树——`git filter-repo` 只重写你指给它的那些提交，密钥躺在**另一个文件**里就会被完整地漏过去，只有把每个可达 blob 都读一遍才知道结果。

退出码为 1 表示有命中，可以直接用来卡住推送。**命中不等于真泄漏**：手工造的测试 Cookie 和文档示例长得跟真凭据一模一样，所以脚本把每条命中（**打码后**）连同路径与 blob 一起打出来，由人判断。占位符（`your-…`、`admin12345`、i18n 里的 `password: "Password"`）会被过滤掉——一个天天误报的扫描器会训练人无视它，那比不扫还糟。

**装成 pre-commit 钩子**（每个 clone 各配一次，只写仓库级配置，不动全局）：

```bash
git config core.hooksPath tools/hooks
```

之后每次提交前会自动跑 `--tree-only`（快），命中就拦下并给出绕过方式。钩子只在**找不到可用 Python 时跳过**而不是拦住——一个因为缺解释器而卡死每次提交的钩子，只会让人条件反射地加 `--no-verify`，等于没装。

> 真出现命中时的处理顺序：**先吊销/轮换那个凭据**，这才是真正堵住口子的动作；重写历史是次要的，而且强推后的旧对象**仍可按 SHA 访问**，要等 GitHub Support 在服务端跑一次 GC 才真正消失。

---

## 部署

生产建议：

1. `go build -ldflags "-s -w"` 出精简二进制，配合 `frontend/dist` 一起丢到服务器
2. 用 systemd / supervisor 常驻，监听 `127.0.0.1:8080`
3. 前置 Nginx 反代，注意 **关闭响应缓冲**，否则 SSE 流式会被攒批：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_buffering off;
    proxy_cache off;
    proxy_read_timeout 600s;
    chunked_transfer_encoding on;
}
```

4. 管理台只对内网开放，或加一层访问控制
5. `data/` 目录做好备份——账号 Cookie 都在里面

### 静态资源缓存

后端对前端产物分了两档，不需要在 Nginx 里额外配：

| 路径 | 响应头 | 原因 |
| --- | --- | --- |
| `/`、SPA 回退路由 | `Cache-Control: no-cache` | 入口 HTML 里写着带 hash 的 bundle 文件名，缓存住会导致重新构建后仍指向已不存在的旧文件 |
| `/assets/*` | `Cache-Control: public, max-age=31536000, immutable` | Vite 的产物名带内容 hash，内容变了文件名就变，可以放心长缓存 |

---

## 常见问题

**Q：账号状态是 `invalid`，错误是 `credential rejected`？**

`__Secure-1PSID` 没了。重新登录 gemini.google.com 取一份新 Cookie，在号池管理里编辑该账号（把新 Cookie 粘进「替换 Cookie」）或重新导入同一行即可。粘贴新 Cookie 会自动重置该账号的续期记录并解除失效状态。

**Q：状态正常，但答案质量明显变差了？**

先看**连通性**那一列是不是琥珀色的「游客额度」。这是 Cookie 静默失效的典型表现——请求全部 200，但按匿名额度作答。点「刷新 Cookie」或重新粘贴。

**Q：Cookie 刷新一列显示「已限流」，是账号坏了吗？**

不是。限流说的是**调用方太频繁**，账号本身没问题。它会自动重试，也不会增加失败计数、不会导致退役。如果你看到大面积限流，把「设置 → Cookie 续期 → 轮换间隔」调大一些。

**Q：`refreshStatus` 是「已失效」并且连续失败 3 次？**

`__Secure-1PSID` 被吊销了（改了密码、在别处退出登录、Google 主动失效）。重新登录取新 Cookie 重新录入。

**Q：添加账号时报「请粘贴 gemini.google.com 的 Cookie，或选择「游客」类型」？**

类型选了 `Cookie` 但 Cookie 是空的。这是**故意**报错的——如果这里悄悄回落成游客账号，你会看到一个「添加成功」而每个请求都在用匿名额度。真要游客就在类型里选 `游客`。

**Q：提示「缺少 __Secure-1PSID」？**

你只复制了 `__Secure-1PSIDTS`。它不是会话本身，单独一条不能认证。回到 `Application → Cookies`，把 `__Secure-1PSID` 一起复制。

**Q：请求返回 200，但正文是空的？**

先看日志里有没有 `frame resync` 之类的重同步计数。空正文几乎总是**分帧解析错位**，不是账号问题——解析器一旦错位就再也找不到帧，于是「成功但没内容」。详见[流式解析](#长度前缀只当提示不当事实)：声明长度只是提示，边界对不上会按括号配平恢复。如果每一帧都在重同步，说明上游又换了计数方式，去 `frame.go` 更新 `frameBoundaryOK` 的判据。

**Q：中文回复出问题，英文正常？**

同一类症状的强信号。字节数与 UTF-16 单位数只在非 ASCII 文本上分叉，所以「英文好的、中文空的」基本可以定位到分帧或长度计算。

**Q：`upstream.baseURL` 里要不要带 `/app`？**

不要。填 `https://gemini.google.com` 就行，`/app` 是初始化时自己拼的路径。老版本默认值带过 `/app`，会拼成 `/app/app`；现在加载时会自动改回来，不用手清。

**Q：流式响应是一坨出来的，不是逐字？**

反代开了响应缓冲。参考上面的 Nginx 配置关掉 `proxy_buffering`。

**Q：一直返回 `1060` 或地区受限？**

出口 IP 在 Gemini 不支持的地区。在「设置 → 上游 → 出口代理」填一个境外代理（支持 `http://` 和 `socks5://`）。只作用于出站的上游请求，不影响管理台访问。

> 排查时注意别被本地环境骗了：Windows / Git Bash 下如果设了 `http_proxy`，**连本机回环的请求也会被塞给代理**，`curl` 会显示连不上（`000`），看着像服务没起来。测本地端口记得加 `--noproxy '*'`。
>
> 网关自己不会犯这个错：**发往回环地址的上游请求一律绕过代理**（`127.0.0.0/8`、`::1`、`localhost`）。代理是用来上外网的，而 `127.0.0.1` 按定义不在外网上；把它塞过去只会拿回一个**空 body 的 `502`**，同样读起来像上游挂了。私有网段（`10.x` 等）**不**绕过——那些地址往往正是要靠代理才能到达。

**Q：`1050` / `1052` 是什么？**

模型编号和请求头说的不是同一件事（`1050`），或者账号的订阅撑不起所选的模型（`1052`）。免费号选 `gemini-3.1-pro` 容易撞上这条——上游有时不是报错而是**静默降级**，所以「明明选了 Pro，回来看是 Flash 的味儿」也属于这一类。

**Q：HTTP 405？**

build label 过期了。适配器会自动重拉 `/app` 并重试一次；如果持续 405，检查上游地址是否被改到了非预期的地方。

**Q：我之前一直用 `gemini-flash`，升级后要改客户端吗？**

不用。项目早期公布的这些名字（`gemini-flash`、`gemini-flash-plus`、`gemini-pro`…）现在都是别名，仍然解析得到，只是指向的模型变成了对应的当前版本（`gemini-flash` → `gemini-3.8-flash`）。`/v1/models` 里列的是新的营销名——客户端如果是从接口拉列表再让人选，会看到新名字。

**Q：日志里出现 `1037` 配额用尽？**

当天额度用完了。账号会进入冷却，冷却结束后自动恢复；也可以加更多账号分摊。

**Q：日志里出现 `1016` 未认证？**

Cookie 已不是有效会话。这条错误会把账号标记为 `invalid`，需要重新录入。

**Q：生成的图片存哪了？**

默认转存到 `data/generated/`，通过 `/media/` 对外提供。可在系统设置里改目录、公开前缀和容量上限。

**Q：图像生成接口的 `n` 参数不生效？**

Gemini 没有独立的图像模型——它在**普通对话里画图**，请求是文字、回答里带媒体。所以这个接口复用默认对话模式，返回几张由提示词决定。`n`、`size` 都是接收但不使用（保留是为了兼容客户端）。

**Q：想换调度策略？**

管理台 → 系统设置 → 路由 → 调度策略。单人用推荐 `least_inflight`，多账号均匀分摊用 `round_robin`。

**Q：升级之后，旧版本留下的一些设置好像还是老样子？**

这是**设计使然，而且是坑**。全新安装会把整套默认设置写进 `app.json`，而 `Normalize` 只补缺失的值、不动已存在的值——所以改一个默认值只影响新安装，存量实例会带着旧值继续跑，且完全没有症状。

修法是**迁移**：`Normalize` 里维护一张已知坏值的清单，命中就改回默认并**写回磁盘**。以后再加默认值修复，记得同时加进那张清单，否则修了等于没修。详见「持久化设计」。

**Q：点「保存」添加账号没反应，只弹一个「此项必填」？**

名称是**可选**的：留空会按 Cookie 派生出的账号标识自动命名，编辑时留空则保持原名。唯一必填的是 Cookie——而且只在类型选了 `Cookie` 的时候。

> 这类「后端说可选、前端说必填」的分歧，路由级和字段级的检查都发现不了——每个页面都渲染正常，字段也都在。所以 `tools/render.mjs` 里有一段**流程检查**：真的开对话框、只粘 Cookie、点保存，断言账号数 +1 且弹窗关闭；反向也查一次——选了 Cookie 类型却不粘任何东西就保存，必须被拒、账号数不变。否则账号会被建成游客，而操作员以为自己刚加了一个登录账号。

---

## 免责声明

本项目仅用于**学习与技术研究**，对接的是第三方服务的 Web 端接口，而该服务并未提供公开 API。使用者需自行确保其使用方式符合目标服务的服务条款及所在地法律法规，因使用本项目产生的任何后果由使用者自行承担。

需要明确知道的几件事：

- **协议是从网页端行为逆向出来的**，上游随时可能改版。届时本项目会失效——这不是 bug，是这类项目的固有属性。
- **批量使用账号可能触发上游的风控**，导致账号被限制或封禁。请只使用你自己的账号，并自行评估风险。
- **自动轮换 Cookie 同理**。它是维持会话的技术手段，但把一批账号长期挂在一个网关后面，本质上就是自动化操作多账号，请自行判断这在你所处的场景下是否合适。
- 请勿用于**商业转售、二次分发额度**或任何绕过付费的用途。
- 本项目与 Google / Gemini 官方无任何关联，未获其授权或认可。

---

## 许可证

本项目以 **GNU Affero General Public License v3.0（AGPL-3.0）** 发布，协议全文见 [LICENSE](LICENSE)。

```
Copyright (C) 2026 juangchuank-ops
```

你可以自由使用、修改、分发本项目，前提是：

- **改了要公开**：若你把修改后的版本通过网络对外提供服务（不只是分发二进制），必须把修改后的完整源码同样以 AGPL-3.0 提供给使用者。
- **保留声明**：分发时保留版权声明与许可证文本。

需要说明的一点：上面免责声明里写的「请勿用于商业转售」是**作者意图的请求**，不是许可证的附加限制。AGPL-3.0 本身允许商业使用——它约束的是**闭源**，而不是**收费**。真正拦得住的是「拿去做闭源服务」，拦不住「拿去卖」。若这与预期不符，需要另行选择非商业许可，但那样会让「能否复用」变得不明确，请自行权衡。
