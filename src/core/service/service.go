package service

import (
	"core"
	"core/config"
	"core/consts/serviceType"
	. "core/libs"
	"core/libs/argv"
	"core/libs/common"
	"core/libs/consul"
	"core/libs/dict"
	"core/libs/grpc/ipc"
	"core/libs/logger"
	"core/libs/mysql"
	"core/libs/redis"
	"core/libs/rpc"
	"core/libs/stack"
	"core/libs/timer"
	"core/libs/websocket"
	"core/messages"
	"github.com/astaxie/beego"
	"net/http"
	_ "net/http/pprof"
	"runtime"
)

type Service struct {
	env  string
	name string
	id   int

	ip    string
	ports map[string]string

	ipcServer *ipc.Server

	ipcClients   map[string]*ipc.Client
	rpcClients   map[string]*rpc.Client
	redisClients map[string]*redis.Client
	dbClients    map[string]*mysql.Client

	wsServer *websocket.Server
}

func NewService(name string) *Service {
	service := &Service{
		name:  name,
		ip:    common.GetLocalIp(),
		ports: make(map[string]string),
	}
	service.init()

	core.Service = service
	return service
}

func (this *Service) init() {
	//错误捕获
	recoverErr()

	//初始化: 使用CPU数设置
	initMaxProcs()

	//初始化: 命令行参数
	initArgv(this)

	//初始化: 配置文件
	initConfig(this)

	//初始化: log
	initLog(this)

	//系统环境输出
	printEnv(this)
}

func initMaxProcs() {
	//runtime.GOMAXPROCS(1)
}

func initArgv(service *Service) {
	err := argv.Init()
	CheckError(err)

	service.env = argv.Values.Env
	service.id = argv.Values.ServiceId
}

func initConfig(service *Service) {
	config.Init(service.env)
}

func initLog(service *Service) {
	logConfig := config.GetLogConfig()

	logOpenDebug := dict.GetBool(logConfig, "debug")
	logOutput := dict.GetString(logConfig, "output")
	logFileName := service.name + "-" + NumToString(service.id)

	logger.SetLogFile(logFileName, logOutput)
	logger.SetLogDebug(logOpenDebug)
}

func printEnv(service *Service) {
	INFO("使用CPU数量:", runtime.GOMAXPROCS(-1))
	INFO("初始GoroutineNum:", runtime.NumGoroutine())
	INFO("服务平台:", service.env)
	INFO("服务名称:", service.name)
	INFO("服务ID:", service.id)

	timer.DoTimer(20*1000, func() {
		INFO("当前GoroutineNum:", runtime.NumGoroutine())
	})
}

func recoverErr() {
	stack.PrintPanicStackError()
}

func packageServiceName(serviceType string, serviceName string) string {
	return "<" + serviceType + ">" + serviceName
}

func (this *Service) registerService(serviceType string, servicePort string) {
	if _, exists := this.ports[serviceType]; exists {
		ERR("该类型的Service已经在本进程内启用", serviceType)
		return
	}

	//注册到Consul
	serviceName := packageServiceName(serviceType, this.name)
	err := consul.NewServive(this.ip, serviceName, this.id, servicePort)
	CheckError(err)

	INFO("join consul service...", serviceName, servicePort)

	//记录该进程启用的端口号
	this.ports[serviceType] = servicePort
}

/*********************************====以下为公开函数====*******************************/

func (this *Service) StartRedis() {
	this.redisClients = make(map[string]*redis.Client)

	redisConfigs := config.GetRedisConfig()
	for aliasName, redisConfig := range redisConfigs {
		client, err := redis.NewClient(redisConfig.(map[string]interface{}))
		CheckError(err)

		this.redisClients[aliasName] = client
		INFO("redis_" + aliasName + "连接成功...")
	}
}

func (this *Service) StartMysql() {
	this.dbClients = make(map[string]*mysql.Client)

	mysqlConfigs := config.GetMysqlConfig()
	index := 0
	for key, mysqlConfig := range mysqlConfigs {
		dbAliasName := key
		if index == 0 {
			dbAliasName = "default"
		}
		index++

		client, err := mysql.NewClient(dbAliasName, mysqlConfig.(map[string]interface{}))
		CheckError(err)

		this.dbClients[key] = client
		INFO("mysql_" + key + "连接成功...")
	}
}

func (this *Service) StartHttpServer() {
	//Api服务配置
	serviceConfig := config.GetApiService(this.id)
	port := dict.GetInt(serviceConfig, "clientPort")
	useSSL := dict.GetBool(serviceConfig, "useSSL")

	//Http服务配置
	if useSSL {
		tslCrt := config.GetApiServiceTslCrt()
		tslKey := config.GetApiServiceTslKey()

		beego.BConfig.Listen.EnableHTTPS = true
		beego.BConfig.Listen.HTTPSCertFile = tslCrt
		beego.BConfig.Listen.HTTPSKeyFile = tslKey
		beego.BConfig.Listen.HTTPSPort = port
	} else {
		beego.BConfig.Listen.HTTPPort = port
	}
	beego.BConfig.RunMode = beego.PROD

	//启动http服务
	go beego.Run()

	//服务注册
	this.registerService(ServiceType.HTTP, NumToString(port))
}

func (this *Service) RegisterHttpRouter(rootPath string, controller beego.ControllerInterface) {
	beego.Router(rootPath, controller)
}

func (this *Service) StartWebSocket() {
	//WebSocket配置
	serviceConfig := config.GetConnectorService(this.id)
	port := dict.GetString(serviceConfig, "clientPort")
	useSSL := dict.GetBool(serviceConfig, "useSSL")

	//创建WebSocket Server
	server := websocket.NewServer(port, this.id)
	if useSSL {
		tslCrt := config.GetConnectorServiceTslCrt()
		tslKey := config.GetConnectorServiceTslKey()
		server.SetTLS(tslCrt, tslKey)
	}
	server.SetSessionMsgHandle(messages.FontReceive)
	server.Start()
	server.StartPing()

	//服务注册
	this.registerService(ServiceType.WS, port)

	//service中保存wsServer
	this.wsServer = server
}

func (this *Service) SetSessionCreateHandle(handle websocket.SessionCreateHandle) {
	if this.wsServer == nil {
		return
	}
	this.wsServer.SetSessionCreateHandle(handle)
}

func (this *Service) StartIpcClient(serviceNames []string) {
	this.ipcClients = make(map[string]*ipc.Client)

	//初始化consul客户端
	consulClient, err := consul.NewClient()
	CheckError(err)

	//初始化Ipc客户端
	for _, serviceName := range serviceNames {
		serviceName = packageServiceName(ServiceType.IPC, serviceName)
		this.ipcClients[serviceName] = ipc.NewClient(consulClient, serviceName, messages.IpcClientReceive)
		INFO("ipc client start...", serviceName)
	}
}

func (this *Service) StartIpcServer() {
	//开启ipcServer
	ipcServer, port, err := ipc.InitServer(messages.IpcServerReceive)
	CheckError(err)
	INFO("ipc server start...", port)

	//service中记录ipcServer
	this.ipcServer = ipcServer

	//服务注册
	this.registerService(ServiceType.IPC, port)
}

func (this *Service) StartRpcClient(serviceNames []string) {
	this.rpcClients = make(map[string]*rpc.Client)

	//初始化consul客户端
	consulClient, err := consul.NewClient()
	CheckError(err)

	//初始化Rpc客户端
	for _, serviceName := range serviceNames {
		serviceName = packageServiceName(ServiceType.RPC, serviceName)
		this.rpcClients[serviceName] = rpc.NewClient(consulClient, serviceName)
		INFO("rpc client start...", serviceName)
	}
}

func (this *Service) StartRpcServer() {
	//开启rpcServer
	port, err := rpc.InitServer()
	CheckError(err)
	INFO("rpc server start...." + port)

	//服务注册
	this.registerService(ServiceType.RPC, port)
}

func (this *Service) RegisterRpcModule(rpcName string, rpcModule interface{}) {
	//rpc模块注册
	err := rpc.RegisterModule(rpcName, rpcModule)
	CheckError(err)
}

func (this *Service) StartPProf(port int) {
	port = port + this.id
	go func() {
		defer stack.PrintPanicStackError()
		http.ListenAndServe(":"+NumToString(port), nil)
	}()
	INFO("debug start...", port)
}

func (this *Service) Env() string {
	return this.env
}

func (this *Service) ID() int {
	return this.id
}

func (this *Service) Name() string {
	return this.name
}

func (this *Service) Ip() string {
	return this.ip
}

func (this *Service) Port(serviceType string) string {
	return this.ports[serviceType]
}

func (this *Service) Identify() string {
	return this.ip + "_" + this.name + "_" + NumToString(this.id)
}

func (this *Service) GetIpcClient(serviceName string) *ipc.Client {
	serviceName = packageServiceName(ServiceType.IPC, serviceName)
	client, _ := this.ipcClients[serviceName]
	return client
}

func (this *Service) GetRpcClient(serviceName string) *rpc.Client {
	serviceName = packageServiceName(ServiceType.RPC, serviceName)
	client, _ := this.rpcClients[serviceName]
	return client
}

func (this *Service) GetRedisClient(redisAliasName string) *redis.Client {
	client, _ := this.redisClients[redisAliasName]
	return client
}

func (this *Service) GetMysqlClient(dbAliasName string) *mysql.Client {
	client, _ := this.dbClients[dbAliasName]
	return client
}

func (this *Service) GetIpcServer() *ipc.Server {
	return this.ipcServer
}
