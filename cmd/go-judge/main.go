// Command go-judge will starts a http server that receives command to run
// programs inside a sandbox.
package main

import (
	"context"
	crypto_rand "crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/net"
	"log"
	math_rand "math/rand"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/criyle/go-judge/cmd/go-judge/config"
	grpcexecutor "github.com/criyle/go-judge/cmd/go-judge/grpc_executor"
	restexecutor "github.com/criyle/go-judge/cmd/go-judge/rest_executor"
	"github.com/criyle/go-judge/cmd/go-judge/version"
	wsexecutor "github.com/criyle/go-judge/cmd/go-judge/ws_executor"
	"github.com/criyle/go-judge/env"
	"github.com/criyle/go-judge/env/pool"
	"github.com/criyle/go-judge/envexec"
	"github.com/criyle/go-judge/filestore"
	"github.com/criyle/go-judge/pb"
	"github.com/criyle/go-judge/worker"
	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_auth "github.com/grpc-ecosystem/go-grpc-middleware/auth"
	grpc_zap "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	ginprometheus "github.com/zsais/go-gin-prometheus"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var logger *zap.Logger

func main() {
	conf := loadConf()
	if conf.Version {
		fmt.Print(version.Version)
		return
	}
	initLogger(conf)
	defer logger.Sync()
	logger.Sugar().Infof("config loaded: %+v", conf)
	initRand()
	warnIfNotLinux()

	// Init environment pool
	fs, fsCleanUp := newFilsStore(conf)
	b, builderParam := newEnvBuilder(conf)
	envPool := newEnvPool(b, conf.EnableMetrics)
	prefork(envPool, conf.PreFork)
	work := newWorker(conf, envPool, fs)
	work.Start()
	logger.Sugar().Infof("Started worker with parallelism=%d, workdir=%s, timeLimitCheckInterval=%v",
		conf.Parallelism, conf.Dir, conf.TimeLimitCheckerInterval)

	servers := []initFunc{
		cleanUpWorker(work),
		cleanUpFs(fsCleanUp),
		initHTTPServer(conf, work, fs, builderParam),
		initMonitorHTTPServer(conf),
		initGRPCServer(conf, work, fs),
	}

	// Gracefully shutdown, with signal / HTTP server / gRPC server / Monitor HTTP server
	sig := make(chan os.Signal, 1+len(servers))

	// worker and fs clean up func
	stops := []stopFunc{}
	for _, s := range servers {
		start, stop := s()
		if start != nil {
			go func() {
				start()
				sig <- os.Interrupt
			}()
		}
		if stop != nil {
			stops = append(stops, stop)
		}
	}

	// background force GC worker
	newForceGCWorker(conf)

	// Graceful shutdown...
	signal.Notify(sig, os.Interrupt)
	<-sig
	signal.Reset(os.Interrupt)

	logger.Sugar().Info("Shutting Down...")

	ctx, cancel := context.WithTimeout(context.TODO(), time.Second*3)
	defer cancel()

	var eg errgroup.Group
	for _, s := range stops {
		s := s
		eg.Go(func() error {
			return s(ctx)
		})
	}

	go func() {
		logger.Sugar().Info("Shutdown Finished ", eg.Wait())
		cancel()
	}()
	<-ctx.Done()
}

func warnIfNotLinux() {
	if runtime.GOOS != "linux" {
		logger.Sugar().Warn("Platform is ", runtime.GOOS)
		logger.Sugar().Warn("Please notice that the primary supporting platform is Linux")
		logger.Sugar().Warn("Windows and macOS(darwin) support are only recommended in development environment")
	}
}

func loadConf() *config.Config {
	var conf config.Config
	if err := conf.Load(); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		log.Fatalln("load config failed ", err)
	}
	return &conf
}

type stopFunc func(ctx context.Context) error
type initFunc func() (start func(), cleanUp stopFunc)

func cleanUpWorker(work worker.Worker) initFunc {
	return func() (start func(), cleanUp stopFunc) {
		return nil, func(ctx context.Context) error {
			work.Shutdown()
			logger.Sugar().Info("Worker shutdown")
			return nil
		}
	}
}

func cleanUpFs(fsCleanUp func() error) initFunc {
	return func() (start func(), cleanUp stopFunc) {
		if fsCleanUp == nil {
			return nil, nil
		}
		return nil, func(ctx context.Context) error {
			err := fsCleanUp()
			logger.Sugar().Info("FileStore cleaned up")
			return err
		}
	}
}

func initHTTPServer(conf *config.Config, work worker.Worker, fs filestore.FileStore, builderParam map[string]any) initFunc {
	return func() (start func(), cleanUp stopFunc) {
		// Init http handle
		r := initHTTPMux(conf, work, fs, builderParam)
		srv := http.Server{
			Addr:    conf.HTTPAddr,
			Handler: r,
		}

		return func() {
				lis, err := newListener(conf.HTTPAddr)
				if err != nil {
					logger.Sugar().Error("Http server listen failed: ", err)
					return
				}
				logger.Sugar().Info("Starting http server at ", conf.HTTPAddr, " with listener ", printListener(lis))
				if err := srv.Serve(lis); errors.Is(err, http.ErrServerClosed) {
					logger.Sugar().Info("Http server stopped: ", err)
				} else {
					logger.Sugar().Error("Http server stopped: ", err)
				}
			}, func(ctx context.Context) error {
				logger.Sugar().Info("Http server shutdown")
				return srv.Shutdown(ctx)
			}
	}
}

func initMonitorHTTPServer(conf *config.Config) initFunc {
	return func() (start func(), cleanUp stopFunc) {
		// Init monitor HTTP server
		mr := initMonitorHTTPMux(conf)
		if mr == nil {
			return nil, nil
		}
		msrv := http.Server{
			Addr:    conf.MonitorAddr,
			Handler: mr,
		}
		return func() {
				lis, err := newListener(conf.MonitorAddr)
				if err != nil {
					logger.Sugar().Error("Monitoring http listen failed: ", err)
					return
				}
				logger.Sugar().Info("Starting monitoring http server at ", conf.MonitorAddr, " with listener ", printListener(lis))
				logger.Sugar().Info("Monitoring http server stopped: ", msrv.Serve(lis))
			}, func(ctx context.Context) error {
				logger.Sugar().Info("Monitoring http server shutdown")
				return msrv.Shutdown(ctx)
			}
	}
}

func initGRPCServer(conf *config.Config, work worker.Worker, fs filestore.FileStore) initFunc {
	return func() (start func(), cleanUp stopFunc) {
		if !conf.EnableGRPC {
			return nil, nil
		}
		// Init gRPC server
		esServer := grpcexecutor.New(work, fs, conf.SrcPrefix, logger)
		grpcServer := newGRPCServer(conf, esServer)

		return func() {
				lis, err := newListener(conf.GRPCAddr)
				if err != nil {
					logger.Sugar().Error("gRPC listen failed: ", err)
					return
				}
				logger.Sugar().Info("Starting gRPC server at ", conf.GRPCAddr, " with listener ", printListener(lis))
				logger.Sugar().Info("gRPC server stopped: ", grpcServer.Serve(lis))
			}, func(ctx context.Context) error {
				grpcServer.GracefulStop()
				logger.Sugar().Info("GRPC server shutdown")
				return nil
			}
	}
}

func initLogger(conf *config.Config) {
	if conf.Silent {
		logger = zap.NewNop()
		return
	}

	var err error
	if conf.Release {
		logger, err = zap.NewProduction()
	} else {
		config := zap.NewDevelopmentConfig()
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		if !conf.EnableDebug {
			config.Level.SetLevel(zap.InfoLevel)
		}
		logger, err = config.Build()
	}
	if err != nil {
		log.Fatalln("init logger failed ", err)
	}
}

func initRand() {
	var b [8]byte
	_, err := crypto_rand.Read(b[:])
	if err != nil {
		logger.Fatal("random generator init failed ", zap.Error(err))
	}
	sd := int64(binary.LittleEndian.Uint64(b[:]))
	logger.Sugar().Infof("random seed: %d", sd)
	math_rand.Seed(sd)
}

func prefork(envPool worker.EnvironmentPool, prefork int) {
	if prefork <= 0 {
		return
	}
	logger.Sugar().Info("create ", prefork, " prefork containers")
	m := make([]envexec.Environment, 0, prefork)
	for i := 0; i < prefork; i++ {
		e, err := envPool.Get()
		if err != nil {
			log.Fatalln("prefork environment failed ", err)
		}
		m = append(m, e)
	}
	for _, e := range m {
		envPool.Put(e)
	}
}

func initHTTPMux(conf *config.Config, work worker.Worker, fs filestore.FileStore, builderParam map[string]any) http.Handler {
	var r *gin.Engine
	if conf.Release {
		gin.SetMode(gin.ReleaseMode)
	}
	r = gin.New()
	r.Use(ginzap.Ginzap(logger, "", false))
	r.Use(ginzap.RecoveryWithZap(logger, true))

	// Metrics Handle
	if conf.EnableMetrics {
		initGinMetrics(r)
	}

	// Version handle
	r.GET("/version", generateHandleVersion(conf, builderParam))

	// Config handle
	r.GET("/config", generateHandleConfig(conf, builderParam))

	r.GET("/checkInfo", generateHandleCheckInfo(conf, builderParam))

	// Add auth token
	if conf.AuthToken != "" {
		r.Use(tokenAuth(conf.AuthToken))
		logger.Sugar().Info("Attach token auth with token: ", conf.AuthToken)
	}

	// Rest Handle
	restHandle := restexecutor.New(work, fs, conf.SrcPrefix, logger)
	restHandle.Register(r)

	// WebSocket Handle
	wsHandle := wsexecutor.New(work, conf.SrcPrefix, logger)
	wsHandle.Register(r)

	return r
}

func initMonitorHTTPMux(conf *config.Config) http.Handler {
	if !conf.EnableMetrics && !conf.EnableDebug {
		return nil
	}
	mux := http.NewServeMux()
	if conf.EnableMetrics {
		mux.Handle("/metrics", promhttp.Handler())
	}
	if conf.EnableDebug {
		initDebugRoute(mux)
	}
	return mux
}

func initDebugRoute(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}

func newGRPCServer(conf *config.Config, esServer pb.ExecutorServer) *grpc.Server {
	grpc_zap.ReplaceGrpcLoggerV2(logger)
	streamMiddleware := []grpc.StreamServerInterceptor{
		grpc_prometheus.StreamServerInterceptor,
		grpc_zap.StreamServerInterceptor(logger),
		grpc_recovery.StreamServerInterceptor(),
	}
	unaryMiddleware := []grpc.UnaryServerInterceptor{
		grpc_prometheus.UnaryServerInterceptor,
		grpc_zap.UnaryServerInterceptor(logger),
		grpc_recovery.UnaryServerInterceptor(),
	}
	if conf.AuthToken != "" {
		authFunc := grpcTokenAuth(conf.AuthToken)
		streamMiddleware = append(streamMiddleware, grpc_auth.StreamServerInterceptor(authFunc))
		unaryMiddleware = append(unaryMiddleware, grpc_auth.UnaryServerInterceptor(authFunc))
	}
	grpcServer := grpc.NewServer(
		grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(streamMiddleware...)),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(unaryMiddleware...)),
	)
	pb.RegisterExecutorServer(grpcServer, esServer)
	grpc_prometheus.Register(grpcServer)
	grpc_prometheus.EnableHandlingTimeHistogram()
	return grpcServer
}

func initGinMetrics(r *gin.Engine) {
	p := ginprometheus.NewPrometheus("gin")
	p.ReqCntURLLabelMappingFn = func(c *gin.Context) string {
		return c.FullPath()
	}
	r.Use(p.HandlerFunc())
}

func tokenAuth(token string) gin.HandlerFunc {
	const bearer = "Bearer "
	return func(c *gin.Context) {
		reqToken := c.GetHeader("Authorization")
		if strings.HasPrefix(reqToken, bearer) && reqToken[len(bearer):] == token {
			c.Next()
			return
		}
		c.AbortWithStatus(http.StatusUnauthorized)
	}
}

func grpcTokenAuth(token string) func(context.Context) (context.Context, error) {
	return func(ctx context.Context) (context.Context, error) {
		reqToken, err := grpc_auth.AuthFromMD(ctx, "bearer")
		if err != nil {
			return nil, err
		}
		if reqToken != token {
			return nil, status.Errorf(codes.Unauthenticated, "invalid auth token: %v", err)
		}
		return ctx, nil
	}
}

func newFilsStore(conf *config.Config) (filestore.FileStore, func() error) {
	const timeoutCheckInterval = 15 * time.Second
	var cleanUp func() error

	var fs filestore.FileStore
	if conf.Dir == "" {
		if runtime.GOOS == "linux" {
			conf.Dir = "/dev/shm"
		} else {
			conf.Dir = os.TempDir()
		}
		var err error
		conf.Dir, err = os.MkdirTemp(conf.Dir, "go-judge")
		if err != nil {
			logger.Sugar().Fatal("failed to create file store temp dir", err)
		}
		cleanUp = func() error {
			return os.RemoveAll(conf.Dir)
		}
	}
	os.MkdirAll(conf.Dir, 0755)
	fs = filestore.NewFileLocalStore(conf.Dir)
	if conf.EnableDebug {
		fs = newMetricsFileStore(fs)
	}
	if conf.FileTimeout > 0 {
		fs = filestore.NewTimeout(fs, conf.FileTimeout, timeoutCheckInterval)
	}
	return fs, cleanUp
}

func newEnvBuilder(conf *config.Config) (pool.EnvBuilder, map[string]any) {
	b, param, err := env.NewBuilder(env.Config{
		ContainerInitPath:  conf.ContainerInitPath,
		MountConf:          conf.MountConf,
		TmpFsParam:         conf.TmpFsParam,
		NetShare:           conf.NetShare,
		CgroupPrefix:       conf.CgroupPrefix,
		Cpuset:             conf.Cpuset,
		ContainerCredStart: conf.ContainerCredStart,
		EnableCPURate:      conf.EnableCPURate,
		CPUCfsPeriod:       conf.CPUCfsPeriod,
		SeccompConf:        conf.SeccompConf,
		Logger:             logger.Sugar(),
	})
	if err != nil {
		logger.Sugar().Fatal("create environment builder failed ", err)
	}
	if conf.EnableMetrics {
		b = &metriceEnvBuilder{b}
	}
	return b, param
}

func newEnvPool(b pool.EnvBuilder, enableMetrics bool) worker.EnvironmentPool {
	p := pool.NewPool(b)
	if enableMetrics {
		p = &metricsEnvPool{p}
	}
	return p
}

func newWorker(conf *config.Config, envPool worker.EnvironmentPool, fs filestore.FileStore) worker.Worker {
	return worker.New(worker.Config{
		FileStore:             fs,
		EnvironmentPool:       envPool,
		Parallelism:           conf.Parallelism,
		WorkDir:               conf.Dir,
		TimeLimitTickInterval: conf.TimeLimitCheckerInterval,
		ExtraMemoryLimit:      *conf.ExtraMemoryLimit,
		OutputLimit:           *conf.OutputLimit,
		CopyOutLimit:          *conf.CopyOutLimit,
		OpenFileLimit:         uint64(conf.OpenFileLimit),
		ExecObserver:          execObserve,
	})
}

func newForceGCWorker(conf *config.Config) {
	go func() {
		ticker := time.NewTicker(conf.ForceGCInterval)
		for {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			if mem.HeapInuse > uint64(*conf.ForceGCTarget) {
				logger.Sugar().Infof("Force GC as heap_in_use(%v) > target(%v)",
					envexec.Size(mem.HeapInuse), *conf.ForceGCTarget)
				runtime.GC()
				debug.FreeOSMemory()
			}
			<-ticker.C
		}
	}()
}

func generateHandleVersion(conf *config.Config, builderParam map[string]any) func(*gin.Context) {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"buildVersion":      version.Version,
			"goVersion":         runtime.Version(),
			"platform":          runtime.GOARCH,
			"os":                runtime.GOOS,
			"copyOutOptional":   true,
			"pipeProxy":         true,
			"symlink":           true,
			"addressSpaceLimit": true,
		})
	}
}

func generateHandleConfig(conf *config.Config, builderParam map[string]any) func(*gin.Context) {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"copyOutOptional":   true,
			"pipeProxy":         true,
			"symlink":           true,
			"addressSpaceLimit": true,
			"fileStorePath":     conf.Dir,
			"runnerConfig":      builderParam,
		})
	}
}

// 辅助函数，计算网络带宽
func calculateNetworkBandwidth() (float64, float64, error) {
	// 获取初始网络 I/O 统计信息
	ioStatsStart, err := net.IOCounters(true)
	if err != nil {
		return 0, 0, err
	}
	startBytesSent, startBytesRecv := sumIOCounters(ioStatsStart)
	startTime := time.Now()

	// 等待一段时间
	time.Sleep(1 * time.Second)

	// 获取新的网络 I/O 统计信息
	ioStatsEnd, err := net.IOCounters(true)
	if err != nil {
		return 0, 0, err
	}
	endBytesSent, endBytesRecv := sumIOCounters(ioStatsEnd)
	endTime := time.Now()

	// 计算时间间隔，以秒为单位
	interval := endTime.Sub(startTime).Seconds()

	// 计算带宽
	mbpsSent := (float64(endBytesSent-startBytesSent) * 8) / (interval * 1024 * 1024)
	mbpsRecv := (float64(endBytesRecv-startBytesRecv) * 8) / (interval * 1024 * 1024)

	return mbpsSent, mbpsRecv, nil
}

// 用于汇总网络 I/O 的辅助函数
func sumIOCounters(ioStats []net.IOCountersStat) (bytesSent, bytesRecv uint64) {
	for _, stat := range ioStats {
		bytesSent += stat.BytesSent
		bytesRecv += stat.BytesRecv
	}
	return
}

// 辅助函数，计算磁盘 I/O
func calculateDiskIO() (float64, float64, error) {
	// 获取初始磁盘 I/O 统计信息
	ioStatsStart, err := disk.IOCounters()
	if err != nil {
		return 0, 0, err
	}
	startReadBytes, startWriteBytes := sumDiskIO(ioStatsStart)
	startTime := time.Now()

	// 等待一段时间
	time.Sleep(1 * time.Second)

	// 获取新的磁盘 I/O 统计信息
	ioStatsEnd, err := disk.IOCounters()
	if err != nil {
		return 0, 0, err
	}
	endReadBytes, endWriteBytes := sumDiskIO(ioStatsEnd)
	endTime := time.Now()

	// 计算时间间隔，以秒为单位
	interval := endTime.Sub(startTime).Seconds()

	// 计算读写速率并转换为 KB/s
	readRate := (float64(endReadBytes-startReadBytes) / 1024) / interval
	writeRate := (float64(endWriteBytes-startWriteBytes) / 1024) / interval

	return readRate, writeRate, nil
}

// 用于汇总磁盘 I/O 的辅助函数
func sumDiskIO(ioStats map[string]disk.IOCountersStat) (readBytes, writeBytes uint64) {
	for _, stat := range ioStats {
		readBytes += stat.ReadBytes
		writeBytes += stat.WriteBytes
	}
	return
}

func generateHandleCheckInfo(conf *config.Config, builderParam map[string]any) func(*gin.Context) {
	return func(c *gin.Context) {
		var wg sync.WaitGroup
		var cpuUsage []float64
		var memoryInfo *mem.VirtualMemoryStat
		var mbpsSent, mbpsRecv float64
		var readRate, writeRate float64
		var err error

		// 并行获取 CPU 使用率
		wg.Add(1)
		go func() {
			defer wg.Done()
			cpuUsage, err = cpu.Percent(time.Second, true)
			if err != nil {
				cpuUsage = nil
			}
		}()

		// 并行获取内存信息
		wg.Add(1)
		go func() {
			defer wg.Done()
			memoryInfo, err = mem.VirtualMemory()
			if err != nil {
				memoryInfo = nil
			}
		}()

		// 并行获取网络带宽
		wg.Add(1)
		go func() {
			defer wg.Done()
			mbpsSent, mbpsRecv, err = calculateNetworkBandwidth()
			if err != nil {
				mbpsSent, mbpsRecv = 0, 0
			}
		}()

		// 并行获取磁盘 I/O 速率
		wg.Add(1)
		go func() {
			defer wg.Done()
			readRate, writeRate, err = calculateDiskIO()
			if err != nil {
				readRate, writeRate = 0, 0
			}
		}()

		// 等待所有协程完成
		wg.Wait()

		// 验证获取到的数据
		if cpuUsage == nil || memoryInfo == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get system information"})
			return
		}

		physicalCnt, _ := cpu.Counts(false)
		logicalCnt, _ := cpu.Counts(true)

		// 返回所有统计信息
		c.JSON(http.StatusOK, gin.H{
			"cpu_total_usage":       cpuUsage[0],                             // CPU 总使用率
			"cpu_physical_cores":    float64(physicalCnt),                    // 物理核心数
			"cpu_logical_cores":     float64(logicalCnt),                     // 逻辑核心数
			"cpu_core_usage":        cpuUsage[1:],                            // 每个核心的使用率
			"memory_usage_percent":  memoryInfo.UsedPercent,                  // 内存使用百分比
			"memory_used_mb":        float64(memoryInfo.Used) / 1024 / 1024,  // 已使用内存（MB）
			"memory_total_mb":       float64(memoryInfo.Total) / 1024 / 1024, // 总内存（MB）
			"network_upload_mbps":   mbpsSent,                                // 网络上传速率（Mbps）
			"network_download_mbps": mbpsRecv,                                // 网络下载速率（Mbps）
			"disk_read_kbps":        readRate,                                // 磁盘读取速率（KB/s）
			"disk_write_kbps":       writeRate,                               // 磁盘写入速率（KB/s）
		})
	}
}
