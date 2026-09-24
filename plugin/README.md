# STATE Kit 独立插件

Sub2API v0.2.8 的开源 OpenAI OAuth transport 插件，提供逐账号 Pro / Team STATE 配置、旧模式同出口绑定、Cookie 分流、固定业务代理复验、备用票队列、续期、请求环境统一和响应异常守护。

请先阅读 [安装与使用说明](../docs/plugin.md)。配置在「插件管理 → STATE Kit → 配置」，不用修改宿主源码。首次安装需在宿主信任本插件的发布者公钥。

- 插件 ID：`io.github.wangyunjeff.sub2api-state-kit`
- 插件版本：`4.5.0`
- 运行方式：稳定同出口保持打票、复验、业务出口一致；Cookie 分流允许采集和业务出口分离
- Cookie 打票：支持代理生成器 API 或 `http`、`https`、`socks5`、`socks5h` 固定采集代理
- 会话完整性：Cookie 模式必须同时具有 STATE、`session_id` 和至少一个响应 Cookie
- `780` 定向：目标网关支持多选，默认白名单为 `unified-15,unified-88,unified-180`，要求 `__cflb/__oailb` 路由对并按其 Fernet 签发时间维持约 `240` 秒；
- 网关诊断：实时汇总各网关的出现、通过、模型不符和拒绝次数；
- 打票指纹：默认收敛为参考实现验证过的 `codex-tui` 请求形态；
- 持续期与备用票：两种运行方式都可提前准备备用票；新票复验成功前不替换当前票
- 账号显示：已发现账号下拉显示 ID；名称、邮箱、到期时间和额度可在添加后填写并保存
- 第一层代理：直接填写完整 `http`、`https`、`socks5` 或 `socks5h` 地址，不依赖宿主不存在的 IP 管理代理列表
- 账号出口：可选 Sub2 原有代理、固定 provider session 粘性代理，或由插件调用代理生成器 API 取得固定出口
- 暂停账号过滤：HostService v2 返回 `schedulable=false` 时不调度该账号
- 生成器过滤：默认阻止香港 `HK`，未知出口地区同样拒绝；IP 与地区查询合并，地区错误快速换出口
- 上一轮出口复用：可按账号优先复用上次复验成功的生成器出口，失败时自动回退到生成器获取新出口
- 模型输入：可选用常用模型建议，也可直接输入自定义模型
- 实时诊断监听：打开面板才开始，关闭即停止并清空；新事件置顶，不保存历史
- 请求环境统一：可开关替换已存在的日期、时区、`user_location.timezone` 和 `Accept-Language`；默认时区为 `Asia/Singapore`
- 协议：Sub2API 插件协议 / transport / UI Bridge / HostService v2
- 宿主源码基线：官方 `v0.2.8`，提交 `fd80b08c90b55edcad5b00171b53f08721d30da1`
- 当前部署范围：单应用实例
- 默认：所有 STATE 开关关闭，不含任何真实账号或代理配置

## 开发

```bash
go test -race ./...
node --test ui-tests/*.test.cjs
go build -trimpath -o build/state-kit ./cmd/state-kit
```

`internal/pluginapi/v1` 来自上述官方源码的公开契约，保留源码和许可归属。其余实现为本项目独立实现，不包含官方闭源传输插件。许可证沿用本仓库 LGPL-3.0。

发布者签名公钥在 `release/`；签名私钥必须在仓库外。参见 `../scripts/package_plugin.py` 和 `integration/` 中的宿主安装/运行集成测试。
