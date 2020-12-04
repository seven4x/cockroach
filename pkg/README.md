```
├── acceptance 验收测试
├── base 多个层 全局共享数据结构：配置，常量
├── bench 性能测试
├── blobs node本地二进制文件读写等接口，会在cloud包中使用，会在server_sql注册为rpc
├── build node构建信息,日志输出会用到
├── ccl  社区协议代码 通过import _使用，还可以构建没有ccl的二进制版本
├── cli  cobra命令行
├── clusterversion 查询集群功能版本是否支持
├── cmd 调用cli，打包命令行二进制
├── col
├── compose
├── config
├── docs
├── featureflag
├── geo
├── gossip
├── internal
├── jobs
├── keys
├── kv
├── migration
├── release
├── roachpb
├── rpc
├── scheduledjobs
├── security
├── server
├── settings
├── sql
├── sqlmigrations
├── storage
├── testutils
├── ts
├── ui
├── util
└── workload
``` 
