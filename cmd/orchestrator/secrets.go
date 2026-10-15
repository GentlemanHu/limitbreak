package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"quota-session-orchestrator/internal/config"
)

func loadManagementKey(c config.Config) error {
	if os.Getenv(c.ManagementKeyEnv) != "" || c.ManagementKeyFile == "" {
		return nil
	}
	st, err := os.Lstat(c.ManagementKeyFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 4096 {
		return errors.New("管理密钥文件必须为 0600 普通文件且不超过 4096 字节")
	}
	b, err := os.ReadFile(c.ManagementKeyFile)
	if err != nil {
		return err
	}
	key := strings.TrimSpace(string(b))
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return errors.New("管理密钥为空或包含换行")
	}
	return os.Setenv(c.ManagementKeyEnv, key)
}

func setKey(path string) error {
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	if c.ManagementKeyFile == "" {
		return errors.New("配置未指定 management_key_file")
	}
	if _, err := os.Lstat(c.ManagementKeyFile); err == nil {
		return errors.New("管理密钥文件已存在，不覆盖；请手动安全更新")
	} else if !os.IsNotExist(err) {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 4097))
	if err != nil {
		return err
	}
	key := strings.TrimSpace(string(b))
	if len(b) > 4096 || key == "" || strings.ContainsAny(key, "\r\n") {
		return errors.New("管理密钥长度或格式无效")
	}
	if err := os.MkdirAll(filepath.Dir(c.ManagementKeyFile), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(c.ManagementKeyFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(key)
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err != nil {
		return err
	}
	if closed != nil {
		return closed
	}
	fmt.Println("已保存管理密钥，内容未输出。守护器下次启动读取私有文件。")
	return nil
}
