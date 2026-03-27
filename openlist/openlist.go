package openlist

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// 在 openlist.go 定义全局可复用的 Client
var httpClient = &http.Client{
	Timeout: 30 * time.Second, // 根据需要可调整
	Transport: &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	},
}

func normalizePath(p string) string {
	// 替换 NBSP 为普通空格
	return strings.ReplaceAll(p, "\u00A0", "%C2%A0")
}

// 获取截断前缀后的相对路径
func getLocalRelativePath(fullPath string, excludeOption int) string {
	parts := strings.Split(fullPath, "/")
	if len(parts)-1 < excludeOption+1 {
		return ""
	}
	return strings.Join(parts[excludeOption+1:], "/")
}

// ---------------- API 请求与下载部分 ----------------

type FsListItem struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
}

// 获取目录列表
func getFsList(apiURL, targetPath, token string) ([]FsListItem, error) {
	targetPath = normalizePath(targetPath)
	payload := map[string]interface{}{
		"path":     targetPath,
		"password": "",
		"page":     1,
		"per_page": 0,
		"refresh":  false,
	}
	jsonPayload, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			Content []FsListItem `json:"content"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	if res.Code != 200 {
		return nil, fmt.Errorf("API 错误: %d, 信息: %s", res.Code, res.Msg)
	}

	return res.Data.Content, nil
}

// 获取单文件的带 sign 直链
func getFsGetRawURL(apiURL, targetPath, token, protocol, host, port string) string {
	targetPath = normalizePath(targetPath)

	payload := map[string]interface{}{
		"path": targetPath,
	}
	jsonPayload, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var res struct {
		Data struct {
			RawURL string `json:"raw_url"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return ""
	}

	rawURL := res.Data.RawURL

	// 修复直链中可能丢失的端口号
	if rawURL != "" {
		if parsedURL, err := url.Parse(rawURL); err == nil {
			parsedURL.Scheme = protocol
			parsedURL.Host = fmt.Sprintf("%s:%s", host, port)
			rawURL = parsedURL.String()
		}
	}

	return rawURL
}

// 同步下载文件（用于字幕）
func downloadFile(urlStr, destPath, hostStr string) error {
	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Referer", hostStr)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP 状态码异常: %d", resp.StatusCode)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

// ---------------- 核心递归处理部分 ----------------

type Generator struct {
	ListAPIURL    string
	GetAPIURL     string
	Token         string
	Protocol      string
	Host          string
	Port          string
	StrmSavePath  string
	ExcludeOption int
	VideoExts     map[string]bool
	SubExts       map[string]bool
	Generated     map[string]bool
	StrmCount     int
	SubCount      int
}

// 递归遍历并生成 strm / 下载字幕
func (g *Generator) TraverseDirectory(currentPath string) error {
	items, err := getFsList(g.ListAPIURL, currentPath, g.Token)
	if err != nil {
		return fmt.Errorf("获取目录失败 [%s]: %v", currentPath, err)
	}

	for _, item := range items {
		fullPath := path.Join(currentPath, item.Name)

		if item.IsDir {
			err := g.TraverseDirectory(fullPath)
			if err != nil {
				fmt.Printf("警告: 遍历子目录出错 [%s]: %v\n", fullPath, err)
			}
		} else {
			extIndex := strings.LastIndex(item.Name, ".")
			if extIndex == -1 {
				continue
			}
			ext := strings.ToLower(item.Name[extIndex+1:])

			relPath := getLocalRelativePath(fullPath, g.ExcludeOption)
			if relPath == "" {
				continue
			}

			if g.VideoExts[ext] {
				// 处理视频文件 -> 生成 .strm
				rawURL := getFsGetRawURL(g.GetAPIURL, fullPath, g.Token, g.Protocol, g.Host, g.Port)
				if rawURL == "" {
					fmt.Printf("获取直链失败，跳过: %s\n", fullPath)
					continue
				}

				strmRelPath := relPath[:strings.LastIndex(relPath, ".")] + ".strm"
				strmFilePath := filepath.Join(g.StrmSavePath, strmRelPath)
				os.MkdirAll(filepath.Dir(strmFilePath), 0755)

				if err := os.WriteFile(strmFilePath, []byte(rawURL), 0644); err == nil {
					absPath, _ := filepath.Abs(strmFilePath)
					g.Generated[absPath] = true
					g.StrmCount++
					if g.StrmCount%50 == 0 {
						fmt.Printf("已生成 %d 个视频 strm 文件...\n", g.StrmCount)
					}
				}
			} else if g.SubExts[ext] {
				// 处理字幕文件 -> 直接下载
				subFilePath := filepath.Join(g.StrmSavePath, relPath)
				absPath, _ := filepath.Abs(subFilePath)

				// 如果本地已存在该字幕文件，跳过下载以节省带宽，但标记为已处理防误删
				if _, err := os.Stat(subFilePath); err == nil {
					g.Generated[absPath] = true
					continue
				}

				rawURL := getFsGetRawURL(g.GetAPIURL, fullPath, g.Token, g.Protocol, g.Host, g.Port)
				if rawURL == "" {
					continue
				}

				os.MkdirAll(filepath.Dir(subFilePath), 0755)
				fmt.Printf("正在下载字幕文件: %s\n", item.Name)
				if err := downloadFile(rawURL, subFilePath, g.Host); err == nil {
					g.Generated[absPath] = true
					g.SubCount++
					time.Sleep(100 * time.Millisecond)
				} else {
					fmt.Printf("下载字幕失败 [%s]: %v\n", item.Name, err)
				}
			}
		}
	}
	return nil
}

// CheckConnection 预检 Openlist API 的连通性和 Token 的有效性
func (g *Generator) CheckConnection() error {
	// 尝试读取根目录，如果能正常返回（即便为空），说明 API 地址和 Token 都是正确的
	_, err := getFsList(g.ListAPIURL, "/", g.Token)
	if err != nil {
		return fmt.Errorf("Openlist API 连通性或 Token 验证失败: %w", err)
	}
	return nil
}
