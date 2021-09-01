/*
 * @Copyright Reserved By Janusec (https://www.janusec.com/).
 * @Author: U2
 * @Date: 2020-10-20 20:31:07
 * @Last Modified: U2, 2020-10-20 20:31:07
 */

package gateway

import (
	"janusec/data"
	"janusec/models"
	"janusec/utils"
	"time"

	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/load"
	"github.com/shirou/gopsutil/mem"
)

var (
	startTime   = time.Now().Unix()
	concurrency = int64(0)
)

// GetGatewayHealth show CPU MEM Storage
// 获取机器运行参数
func GetGatewayHealth() (models.GateHealth, error) {
	cpuLoad, _ := load.Avg()
	cpuPercent, _ := cpu.Percent(1*time.Second, false)
	if len(cpuPercent) == 0 {
		cpuPercent = []float64{0.0}
		utils.DebugPrintln("GetGatewayHealth cpu.Percent []")
	}
	memStat, _ := mem.VirtualMemory()
	diskStat, _ := disk.Usage("/")
	timeZone, offset := time.Now().Zone()
	gateHealth := models.GateHealth{
		StartTime:   startTime,
		CurrentTime: time.Now().Unix(),
		Version:     data.Version,
		CPUPercent:  cpuPercent[0],
		CPULoad1:    cpuLoad.Load1,
		CPULoad5:    cpuLoad.Load5,
		CPULoad15:   cpuLoad.Load15,
		MemUsed:     memStat.Used,
		MemTotal:    memStat.Total,
		DiskUsed:    diskStat.Used,
		DiskTotal:   diskStat.Total,
		TimeZone:    timeZone,
		TimeOffset:  offset / 3600.0,
		ConCurrency: concurrency,
	}
	return gateHealth, nil
}
