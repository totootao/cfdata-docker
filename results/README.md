# 扫描结果

`scan.yml` Workflow 运行后，此目录会生成各数据中心的结果文件：

- `lhr.csv` / `lhr.txt` — 伦敦（LHR）前十
- `fra.csv` / `fra.txt` — 法兰克福（FRA）前十
- `sea.csv` / `sea.txt` — 西雅图（SEA）前十
- `summary.txt` — 汇总

`.csv` 含完整信息（IP、端口、机房、城市、延迟、速度，按速度降序）；
`.txt` 为纯 `ip:port` 列表，可直接用于客户端订阅/优选。

注意：GitHub 托管 Runner 出口在美国（anycast 就近落地），`SEA` 通常有结果，
`LHR` / `FRA` 需在对应地区网络运行（本地 Docker 或自托管 Runner）才能扫到。
