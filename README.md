# 一、介绍

本项目实现了一个现代C2框架中的核心高级特性——**Beacon 级联代理 (Beacon Cascading)**。通过在边缘出网主机上建立网关 (Gateway)，并利用内部网络协议向内网纵深节点 (Pivot) 逐跳延伸，项目构建出了一条稳定且隐蔽的树状控制代理链。

本项目深入探讨并还原了类似Cobalt Strike、Sliver等高级C2的多级网络通信流向与底层路由逻辑，旨在将受控主机转化为应用层路由器，实现对隔离网段的深度穿透。

因本人水平有限，项目简陋，问题多多，只适合研究学习，不能用于实战！

# 二、使用

## （一）TcpPivot

前提：如果有依赖问题，请使用 `go mod tidy`

①依次运行 `server.go`、`gateway.go`、`pivot1.go`、`pivot2.go`

此时，gateway会主动连接server监听的8888端口，server会有上线记录。

![PixPin_2026-02-27_18-36-41.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-36-43-fb71a806cc8d13b52d4aa08d2a870daf-PixPin_2026-02-27_18-36-41-07f73f.png)

![PixPin_2026-02-27_18-37-00.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-37-02-47e398691bf1f1ff4ee275d5b952332c-PixPin_2026-02-27_18-37-00-466f02.png)

②在server的控制台输入 `connect <agentid> 127.0.0.1:9999` 让gateway连接pivot1的9999端口

成功连接后，server会有上线记录

![PixPin_2026-02-27_18-38-40.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-38-44-edd3fd021777fbdf0e876d5babab1a77-PixPin_2026-02-27_18-38-40-67cb40.png)

![PixPin_2026-02-27_18-39-18.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-39-19-a101f437dd533e8d3c641cace6605d72-PixPin_2026-02-27_18-39-18-2d28de.png)

③在server的控制台输入 `connect <agentid> 127.0.0.1:9999` 让pivot1连接pivot2的10010端口

成功连接后，server会有上线记录

![PixPin_2026-02-27_18-40-18.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-40-22-60949aed45f1664035e949683b2cb42a-PixPin_2026-02-27_18-40-18-a8614e.png)

![PixPin_2026-02-27_18-40-37.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-40-42-2fe6a2faf8b4f929f303af4f4c3a0f61-PixPin_2026-02-27_18-40-37-5883da.png)

④在server的控制台输入 `exec <agentid> whoami` 让pivot2执行whoami命令

![PixPin_2026-02-27_18-42-38.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-42-39-716fbe1cc8cc5796081d37de360b1821-PixPin_2026-02-27_18-42-38-05df2b.png)

![PixPin_2026-02-27_18-42-45.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-42-47-ac3e252af1e5abdb457e3a5f04f104cd-PixPin_2026-02-27_18-42-45-adb24c.png)

## （二）SMBPivot

前提：如果有依赖问题，请使用 `go mod tidy`

①依次运行 `server.go`、`gateway.go`、`pivot1.go`、`pivot2.go`

此时，gateway会主动访问server的`http://localhost:8080/api/beat`，server会有上线记录。

![PixPin_2026-02-27_18-48-31.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-48-34-b0355fb82778905db59e3e5d1423c4eb-PixPin_2026-02-27_18-48-31-59ca24.png)

![PixPin_2026-02-27_18-48-45.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-48-46-ba9d090d6841496b3a78c5a9c3b656e7-PixPin_2026-02-27_18-48-45-ccfeb7.png)

②在server的控制台输入 `link <agentid> \\.\pipe\pivot1` 让gateway连接pivot1创建的命名管道 `\\.\pipe\pivot1`

成功连接后，server会有上线记录

![PixPin_2026-02-27_18-49-35.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-49-37-500a238746b2bcdc6a9b6cd756fed79a-PixPin_2026-02-27_18-49-35-ed1325.png)

![PixPin_2026-02-27_18-50-31.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-50-34-6888e7a2ff7ff0eb79cbfdbcfced8b07-PixPin_2026-02-27_18-50-31-2920a4.png)

③在server的控制台输入 `link <agentid> \\.\pipe\pivot2` 让pivot1连接pivot2创建的命名管道 `\\.\pipe\pivot2`

成功连接后，server会有上线记录

![PixPin_2026-02-27_18-51-29.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-51-31-366e68e6f9ef8da490e1eb6ef30e10e6-PixPin_2026-02-27_18-51-29-7f98eb.png)

![PixPin_2026-02-27_18-51-35.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-51-37-cdf49ef66ca53750c709fafa4c0347d5-PixPin_2026-02-27_18-51-35-8c81f8.png)

④在server的控制台输入 `exec <agentid> whoami` 让pivot2执行whoami命令

![PixPin_2026-02-27_18-52-01.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-52-03-614e8d8f6a993dbd3c4bc219e1da40b1-PixPin_2026-02-27_18-52-01-b0105d.png)

![PixPin_2026-02-27_18-52-06.png](https://images-of-oneday.oss-cn-guangzhou.aliyuncs.com/images%2F2026%2F02%2F27%2F18-52-09-f3f62965e89fa41090d8c49de78072b8-PixPin_2026-02-27_18-52-06-4fead7.png)

# 三、更多细节

如果你对Beacon级联代理的实现原理感兴趣或者想了解更多细节，请前往我的先知个人社区主页：https://xz.aliyun.com/users/144519/news

我的博客：https://onedays12.github.io/

# 四、参考资料

1. CobaltStrike
2. sliver：[BishopFox/sliver: Adversary Emulation Framework](https://github.com/BishopFox/sliver)
3. havoc：[Havoc/payloads/Demon/src/core/Pivot.c at main · HavocFramework/Havoc](https://github.com/HavocFramework/Havoc/blob/main/payloads/Demon/src/core/Pivot.c)
4. AdaptixC2：[Adaptix-Framework/AdaptixC2: AdaptixC2 is a highly modular advanced redteam toolkit](https://github.com/Adaptix-Framework/AdaptixC2)
5. Merlin：[Ne0nd0g/merlin: Merlin is a cross-platform post-exploitation HTTP/2 Command & Control server and agent written in golang.](https://github.com/Ne0nd0g/merlin)
6. Stowaway： [ph4ntonn/Stowaway: 👻Stowaway -- Multi-hop Proxy Tool for pentesters](https://github.com/ph4ntonn/Stowaway) 