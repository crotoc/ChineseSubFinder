package restore_fix_timeline_bk

import (
	"errors"
	"fmt"
	"github.com/allanpk716/ChineseSubFinder/internal/pkg/my_util"
	"os"
	"runtime"
)

// CheckSpeFile 目标是检测特定的文件，找到后，先删除，返回一个标志位用于后面的逻辑
func CheckSpeFile() (bool, error) {

	nowSpeFileName := getSpeFileName()
	if nowSpeFileName == "" {
		return false, errors.New(fmt.Sprintf(`restore_fix_timeline_bk.getSpeFileName() is empty, not support this OS. 
you needd implement getSpeFileName() in internal/logic/restore_fix_timeline_bk/restore_fix_timeline_bk.go`))
	}
	if my_util.IsFile(nowSpeFileName) == false {
		return false, nil
	}
	// 先删除这个文件，然后再标记执行该逻辑
	err := os.Remove(nowSpeFileName)
	if err != nil {
		return false, err
	}

	return true, nil
}

func getSpeFileName() string {
	nowSpeFileName := ""
	sysType := runtime.GOOS
	if sysType == "linux" {
		home, _ := os.UserHomeDir()
		nowSpeFileName = home + specialFileNameLinux
	}
	if sysType == "windows" {
		nowSpeFileName = specialFileNameWindows
	}
	if sysType == "darwin" {
		home, _ := os.UserHomeDir()
		nowSpeFileName = home + "/.config/chinesesubfinder/" + specialFileNameDarwin
	}
	return nowSpeFileName
}

/*
	识别 config 文件夹下面由这个特殊的文件，就会执行从 csf-bk 文件还原时间轴修复前的字幕文件
	对于 Linux 是 /config 文件夹下
	对于 Windows 是程序根目录下
	对于 MacOS 需要自行实现
*/
const (
	specialFileNameWindows = "RestoreFixTimelineBK"
	specialFileNameLinux   = "/config/" + specialFileNameWindows
	specialFileNameDarwin  = "RestoreFixTimelineBK"
)
