# STATE Kit 独立插件

Sub2API v0.2.8 的开源 OpenAI OAuth transport 插件，提供逐账号 Pro / Team Cookie + 780 票据、固定业务代理复验、严格质量探针与临时票保活、备用票队列、续期、请求环境统一和响应异常守护。

请先阅读 [安装与使用说明](../docs/plugin.md)。配置在「插件管理 → STATE Kit → 配置」，不用修改宿主源码。首次安装需在宿主信任本插件的发布者公钥。

- 插件 ID：`io.github.wangyunjeff.sub2api-state-kit`
- 插件版本：`4.7.0`
- 票据模式：正式配置只维护 Cookie + `780`；旧 292/332 配置保存时自动迁移
- Cookie 打票：支持代理生成器 API 或 `http`、`https`、`socks5`、`socks5h` 固定采集代理
- 会话完整性：Cookie 模式必须同时具有 STATE、`session_id` 和响应 Cookie；`780` 还必须同时具有 `__cflb`、`__oailb`，缺少任一项都会拒票重打
- `780` 定向：支持 `allow / deny / any` 三态网关策略，默认 `allow` 和 `unified-15,unified-88,unified-180`，要求 `__cflb/__oailb` 路由对并按其 Fernet 签发时间维持约 `240` 秒；
- 网关诊断：按采集、复验、质量探针阶段汇总，不再显示混合口径的通过率；
- 打票指纹：默认收敛为参考实现验证过的 `codex-tui` 请求形态；
- 质量探针：支持 `strict_fallback / strict / off`；严格优先模式无旧票时可先发布临时票保活，并后台继续寻找严格票
- 并发打票：`1..3` 路共享尝试预算，严格票提交后取消其他请求
- 持续期与备用票：可提前准备备用票；新票复验成功前不替换当前票
- 账号显示：已发现账号下拉显示 ID；名称、邮箱、到期时间和额度可在添加后填写并保存
- 第一层代理：直接填写完整 `http`、`https`、`socks5` 或 `socks5h` 地址，不依赖宿主不存在的 IP 管理代理列表
- 账号出口：由全局打票方式和业务固定代理统一控制，不再显示账号级旧出口选项
- 暂停账号过滤：HostService v2 返回 `schedulable=false` 时不调度该账号
- 生成器过滤：默认阻止香港 `HK`，未知出口地区同样拒绝；IP 与地区查询合并，地区错误快速换出口
- 上一轮出口复用：可按账号优先复用上次复验成功的生成器出口，失败时自动回退到生成器获取新出口
- 模型输入：可选用常用模型建议，也可直接输入自定义模型
- 实时诊断监听：内存保留最近 5 小时、硬上限 5000 条，面板显示最新 2000 条；宿主内全屏单滚动事件面板，关闭面板不清历史，插件重启清空
- 请求环境统一：可开关替换已存在的日期、时区、`user_location.timezone` 和 `Accept-Language`；默认时区为 `Asia/Singapore`
- 协议：Sub2API 插件协议 / transport / UI Bridge / HostService v2
- 宿主源码基线：官方 `v0.2.8`，提交 `fd80b08c90b55edcad5b00171b53f08721d30da1`
- 当前部署范围：单应用实例
- 默认：所有 STATE 开关关闭，不含任何真实账号或代理配置

## 开发

```bash
go test -race ./...
node --test ui-tests/app.test.cjs
go build -trimpath -o build/state-kit ./cmd/state-kit
```

`internal/pluginapi/v1` 来自上述官方源码的公开契约，保留源码和许可归属。其余实现为本项目独立实现，不包含官方闭源传输插件。许可证沿用本仓库 LGPL-3.0。

发布者签名公钥在 `release/`；签名私钥必须在仓库外。参见 `../scripts/package_plugin.py` 和 `integration/` 中的宿主安装/运行集成测试。
