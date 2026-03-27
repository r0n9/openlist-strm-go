package main

import (
	"fmt"
	"github.com/joho/godotenv"
	"openlist-strm-go/openlist"
	"openlist-strm-go/smb"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	// 尝试加载当前目录下的 .env 文件
	// 使用 _ 忽略错误，这样即使用户没有 .env 文件而是通过 Docker 环境变量运行，程序也不会报错
	_ = godotenv.Load()

	// 1. 读取配置
	openlistProtocol := getEnv("OPENLIST_PROTOCOL", "http")
	openlistHost := getEnv("OPENLIST_HOST", "localhost")
	openlistPort := getEnv("OPENLIST_PORT", "5244")
	openlistToken := getEnv("OPENLIST_TOKEN", "")

	scanPathsStr := getEnv("SCAN_PATHS", "/115") // 默认挂载点 /115 支持多个，逗号分割
	strmSavePath := getEnv("STRM_SAVE_PATH", "./data")
	excludeOption := getEnvInt("EXCLUDE_OPTION", 1)
	deleteAbsent := getEnvInt("DELETE_ABSENT", 1)

	// SMB 配置环境变量
	enableSMBSync := getEnvInt("SYNC_TO_SMB", 0) // 默认 0 不开启，1 开启
	smbServer := getEnv("SMB_SERVER", "192.168.1.10")
	smbPort := getEnv("SMB_PORT", "445")
	smbUser := getEnv("SMB_USER", "")
	smbPass := getEnv("SMB_PASS", "")
	smbShare := getEnv("SMB_SHARE", "Media") // SMB 共享文件夹名称，不需要包含路径和 //
	smbDir := getEnv("SMB_DIR", "strm")      // 同步生成的strm文件目录，默认就是：smb://192.168.1.10/Media/strm

	// ==================== 阶段 A：基础配置检查 ====================
	fmt.Println("正在检查基础配置...")
	if openlistToken == "" {
		fmt.Println("❌ 致命错误：未配置 OPENLIST_TOKEN 环境变量！")
		os.Exit(1)
	}
	if strings.TrimSpace(scanPathsStr) == "" {
		fmt.Println("❌ 致命错误：SCAN_PATHS 配置不能为空！")
		os.Exit(1)
	}

	openlistURL := fmt.Sprintf("%s://%s:%s", openlistProtocol, openlistHost, openlistPort)
	apiURLFsList := fmt.Sprintf("%s/api/fs/list", openlistURL)
	apiURLFsGet := fmt.Sprintf("%s/api/fs/get", openlistURL)

	fmt.Println("开始扫描并处理视频流文件及字幕...")

	gen := &openlist.Generator{
		ListAPIURL:    apiURLFsList,
		GetAPIURL:     apiURLFsGet,
		Token:         openlistToken,
		Protocol:      openlistProtocol,
		Host:          openlistHost,
		Port:          openlistPort,
		StrmSavePath:  strmSavePath,
		ExcludeOption: excludeOption,
		VideoExts:     getVideoExtensions(),
		SubExts:       getSubtitleExtensions(),
		Generated:     make(map[string]bool),
		StrmCount:     0,
		SubCount:      0,
	}

	// ==================== 阶段 B：服务连通性测试 ====================
	fmt.Printf("正在测试 Openlist 连通性 (%s)...\n", openlistURL)
	if err := gen.CheckConnection(); err != nil {
		fmt.Printf("❌ Openlist 预检失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✅ Openlist 连接与鉴权成功！")

	if enableSMBSync == 1 {
		fmt.Printf("正在测试 SMB 服务器连通性 (%s:%s)...\n", smbServer, smbPort)
		if err := smb.CheckConnection(smbServer, smbPort, smbUser, smbPass, smbShare); err != nil {
			fmt.Printf("❌ SMB 预检失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✅ SMB 连接、鉴权与挂载成功！")
	}

	// ==================== 阶段 C：正式执行核心业务 ====================
	fmt.Println("\n==============================================")
	fmt.Println("🚀 所有系统检查通过，开始扫描并处理数据...")
	fmt.Println("==============================================")

	scanPaths := strings.Split(scanPathsStr, ",")

	for _, p := range scanPaths {
		targetPath := strings.TrimSpace(p)
		if targetPath == "" {
			continue
		}

		fmt.Printf("\n>>> 正在扫描路径: %s\n", targetPath)

		err := gen.TraverseDirectory(targetPath)
		if err != nil {
			fmt.Printf("⚠️ 扫描路径 %s 时发生错误: %v\n", targetPath, err)
		}
	}

	fmt.Printf("\n扫描结束: 生成了 %d 个视频 strm，新下载了 %d 个字幕文件。\n", gen.StrmCount, gen.SubCount)

	if deleteAbsent == 1 {
		deleteAbsentFiles(strmSavePath, gen.Generated, gen.SubExts)
	}

	// 执行 SMB 同步逻辑
	if enableSMBSync == 1 {
		smb.SyncToSMB(strmSavePath, smbServer, smbPort, smbUser, smbPass, smbShare, smbDir)
	}

	fmt.Println("🎉 处理完毕！")
}

// 清理失效的 .strm 和字幕文件
func deleteAbsentFiles(strmPath string, generatedFiles map[string]bool, subExts map[string]bool) {
	fmt.Println("开始清理本地已失效的 .strm 和字幕文件...")
	filepath.Walk(strmPath, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			ext := strings.ToLower(filepath.Ext(info.Name()))
			extWithoutDot := strings.TrimPrefix(ext, ".")

			// 仅针对 .strm 和支持的字幕格式进行检查和清理
			if ext == ".strm" || subExts[extWithoutDot] {
				absPath, _ := filepath.Abs(p)
				if !generatedFiles[absPath] {
					os.Remove(absPath)
					fmt.Printf("删除多余文件: %s\n", absPath)
				}
			}
		}
		return nil
	})
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if value, exists := os.LookupEnv(key); exists {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return fallback
}

// 仅保留可播放的视频格式
func getVideoExtensions() map[string]bool {
	return map[string]bool{
		"mp4": true,
		"mkv": true,
		"avi": true,
		"mov": true,
	}
}

// 定义需要直接下载的字幕格式
func getSubtitleExtensions() map[string]bool {
	return map[string]bool{
		"srt": true,
		"ass": true,
		"ssa": true,
		"sub": true,
		"vtt": true,
	}
}
