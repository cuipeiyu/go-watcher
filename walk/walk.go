package walk

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
)

// 如果 WalkFunc 返回该错误 则忽略错误继续向下执行
var SkipDir = errors.New("skip this directory")

type WalkFunc func(path string, info os.FileInfo, err error) error

func ReadDirNames(dirname string) ([]string, error) {
	f, err := os.Open(dirname)
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// 仅遍历该目录
func WalkOnce(path string, walkFn WalkFunc) error {
	info, err := os.Lstat(path)
	if err != nil {
		err = walkFn(path, nil, err)
	} else {
		err = walker(path, info, walkFn, true)
	}
	if err == SkipDir {
		return nil
	}
	return err
}

// 递归遍历该目录
func WalkPath(path string, walkFn WalkFunc) error {
	info, err := os.Lstat(path)
	if err != nil {
		err = walkFn(path, nil, err)
	} else {
		err = walker(path, info, walkFn, false)
	}
	if err == SkipDir {
		return nil
	}
	return err
}

func walker(path string, info os.FileInfo, walkFn WalkFunc, once bool) error {
	// 是文件 不用继续了
	if !info.IsDir() {
		return walkFn(path, info, nil)
	}

	names, err := ReadDirNames(path)

	err1 := walkFn(path, info, err)
	if err != nil || err1 != nil {
		return err1
	}

	for _, name := range names {
		filename := filepath.Join(path, name)
		fileInfo, err := os.Lstat(filename)
		if err != nil || once {
			// 出错 则执行用户函数
			if err := walkFn(filename, fileInfo, err); err != nil && err != SkipDir {
				// 用户认为确实是错误则退出walk
				// 如果用户给的错是 SkipDir 则忽略该目录
				return err
			}
		} else {
			err = walker(filename, fileInfo, walkFn, once)
			if err != nil {
				if !fileInfo.IsDir() || err != SkipDir {
					return err
				}
			}
		}
	}
	return nil
}
