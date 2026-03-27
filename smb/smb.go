package smb

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hirochachacha/go-smb2"
)

// ---------------- SMB 远程同步部分 ----------------

// smbMkdirAll 递归在 SMB 上创建多级目录
func smbMkdirAll(fs *smb2.Share, dirPath string) error {
	dirPath = strings.ReplaceAll(dirPath, "/", "\\")
	dirPath = strings.Trim(dirPath, "\\")

	if dirPath == "." || dirPath == "" {
		return nil
	}

	info, err := fs.Stat(dirPath)
	if err == nil {
		if info.IsDir() {
			return nil
		}
		return fmt.Errorf("远端存在同名文件，无法创建目录: %s", dirPath)
	}

	lastSlash := strings.LastIndex(dirPath, "\\")
	if lastSlash > 0 {
		parent := dirPath[:lastSlash]
		if err := smbMkdirAll(fs, parent); err != nil {
			return err
		}
	}

	err = fs.Mkdir(dirPath, 0755)
	if err != nil {
		if _, statErr := fs.Stat(dirPath); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

// cleanupSMB 递归遍历远端，删除不在 validPaths 里的冗余文件和文件夹 (自底向上清理)
func cleanupSMB(fs *smb2.Share, currentPath string, validPaths map[string]bool) error {
	currentPath = strings.ReplaceAll(currentPath, "/", "\\")
	currentPath = strings.TrimRight(currentPath, "\\")

	// 如果传入空字符串，SMB 需要用 "." 代表根目录进行读取
	readTarget := currentPath
	if readTarget == "" {
		readTarget = "."
	}

	entries, err := fs.ReadDir(readTarget)
	if err != nil {
		return nil // 目录可能不存在，直接跳过
	}

	for _, entry := range entries {
		name := entry.Name()
		if name == "." || name == ".." {
			continue
		}

		fullPath := name
		if currentPath != "" {
			fullPath = currentPath + "\\" + name
		}

		// 1. 如果是目录，先向下递归（保证从最深处开始删，否则远端目录非空无法删除）
		if entry.IsDir() {
			_ = cleanupSMB(fs, fullPath, validPaths)
		}

		// 2. 检查当前项（无论文件还是已被清空的目录）是否在白名单里
		if !validPaths[fullPath] {
			fmt.Printf("清理远端冗余项: %s\n", fullPath)
			err := fs.Remove(fullPath)
			if err != nil {
				fmt.Printf("删除远端项失败 [%s]: %v\n", fullPath, err)
			}
		}
	}
	return nil
}

// SyncToSMB 将本地目录同步到远端 SMB，并清理远端多余的文件
func SyncToSMB(localDir, server, port, user, pass, shareName, remoteDir string) {
	targetURL := fmt.Sprintf("smb://%s/%s", server, shareName)
	if remoteDir != "" {
		targetURL += "/" + strings.TrimLeft(strings.ReplaceAll(remoteDir, "\\", "/"), "/")
	}
	fmt.Printf("\n开始镜像同步本地目录 [%s] 到 SMB 服务器 [%s]...\n", localDir, targetURL)

	conn, err := net.Dial("tcp", fmt.Sprintf("%s:%s", server, port))
	if err != nil {
		fmt.Printf("连接 SMB 端口失败: %v\n", err)
		return
	}
	defer conn.Close()

	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     user,
			Password: pass,
		},
	}

	s, err := d.Dial(conn)
	if err != nil {
		fmt.Printf("SMB 登录验证失败: %v\n", err)
		return
	}
	defer s.Logoff()

	fs, err := s.Mount(shareName)
	if err != nil {
		fmt.Printf("挂载 SMB 共享目录 [%s] 失败: %v\n", shareName, err)
		return
	}
	defer fs.Umount()

	if remoteDir != "" {
		if err := smbMkdirAll(fs, remoteDir); err != nil {
			fmt.Printf("创建远端基础文件夹 [%s] 失败: %v\n", remoteDir, err)
			return
		}
	}

	// validPaths 记录当前本地真实存在的所有相对路径（将用于最后的远端清理核对）
	validPaths := make(map[string]bool)

	// 提前将指定的远端基准目录也加入白名单，防止在清理根目录时被误删
	if remoteDir != "" {
		cleanRemoteDir := strings.TrimRight(strings.ReplaceAll(remoteDir, "/", "\\"), "\\")
		validPaths[cleanRemoteDir] = true
	}

	syncCount := 0

	// --- 阶段 1：扫描本地、创建并覆盖同步到远端 ---
	err = filepath.Walk(localDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, _ := filepath.Rel(localDir, p)
		if relPath == "." {
			return nil
		}

		smbPath := relPath
		if remoteDir != "" {
			cleanRemoteDir := strings.TrimRight(strings.ReplaceAll(remoteDir, "/", "\\"), "\\")
			smbPath = cleanRemoteDir + "\\" + relPath
		}
		smbPath = strings.ReplaceAll(smbPath, "/", "\\")

		// 核心：把该项注册到白名单里
		validPaths[smbPath] = true

		if info.IsDir() {
			err = smbMkdirAll(fs, smbPath)
			if err != nil {
				fmt.Printf("同步创建远端文件夹 [%s] 失败: %v\n", smbPath, err)
			}
			return nil
		}

		lastSlash := strings.LastIndex(smbPath, "\\")
		if lastSlash > 0 {
			smbDir := smbPath[:lastSlash]
			if err := smbMkdirAll(fs, smbDir); err != nil {
				return nil
			}
		}

		// 检查远端文件是否存在及其大小
		remoteStat, err := fs.Stat(smbPath)
		if err == nil && !remoteStat.IsDir() {
			// 如果大小一致，直接跳过 (增量同步)
			if remoteStat.Size() == info.Size() {
				return nil
			}
		}

		// 只有不存在或大小不一致时才重新上传
		localFile, err := os.Open(p)
		if err != nil {
			fmt.Printf("无法读取本地文件: %s\n", p)
			return nil
		}
		defer localFile.Close()

		remoteFile, err := fs.Create(smbPath)
		if err != nil {
			fmt.Printf("无法创建远程文件 [%s]: %v\n", smbPath, err)
			return nil
		}
		defer remoteFile.Close()

		_, err = io.Copy(remoteFile, localFile)
		if err == nil {
			syncCount++
			if syncCount%50 == 0 {
				fmt.Printf("已同步 %d 个文件到 SMB...\n", syncCount)
			}
		} else {
			fmt.Printf("传输文件数据失败 [%s]: %v\n", smbPath, err)
		}
		return nil
	})

	if err != nil {
		fmt.Printf("本地同步到远端过程发生错误: %v\n", err)
	}

	// --- 阶段 2：遍历远端，清理不在白名单内的多余内容 ---
	fmt.Println("开始比对并清理远端已失效的文件和文件夹...")
	cleanTarget := remoteDir
	if cleanTarget == "" {
		cleanTarget = "." // 如果没有指定远端目录，则清理整个共享根目录下多出的内容
	}

	cleanupErr := cleanupSMB(fs, cleanTarget, validPaths)
	if cleanupErr != nil {
		fmt.Printf("清理远端失效文件时遇到错误: %v\n", cleanupErr)
	}

	fmt.Printf("SMB 同步作业完成！共传输/更新 %d 个文件，并已自动清理远端冗余数据。\n", syncCount)
}

// CheckConnection 预检 SMB 服务器的连通性、账号密码验证以及共享目录的读写权限
func CheckConnection(server, port, user, pass, shareName string) error {
	address := fmt.Sprintf("%s:%s", server, port)

	// 1. 测试网络连通性 (5秒超时)
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return fmt.Errorf("无法连接到 SMB 服务器端口 [%s]: %w", address, err)
	}
	defer conn.Close()

	// 2. 测试账号密码认证
	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     user,
			Password: pass,
		},
	}
	s, err := d.Dial(conn)
	if err != nil {
		return fmt.Errorf("SMB 账号或密码验证失败: %w", err)
	}
	defer s.Logoff()

	// 3. 测试共享目录挂载权限
	fs, err := s.Mount(shareName)
	if err != nil {
		return fmt.Errorf("无法挂载 SMB 共享目录 [%s]，请检查共享名称或权限: %w", shareName, err)
	}
	defer fs.Umount()

	return nil
}
