// cpu-simulator: 模拟 Qualcomm 骁龙 8 Elite Gen 5 SoC 的 CPU 信息，支持动态频率、温度等数据。
// 用于在无真实硬件的环境中测试 koordlet、cloudphone 等组件。
//
// 用法:
//
//	cpu-simulator -cpu 8 -root /tmp/cpu-sim
//
// 将创建:
//   - root/sys/devices/system/cpu/ 完整结构 (kernel_max, online, possible, present, cpufreq, topology, stats)
//   - root/sys/class/thermal/thermal_zone0/ 等
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 骁龙 8 Elite Gen 5: 2 Prime (4.6GHz) + 6 Performance (3.62GHz)
const (
	primeMinFreq   = 576000   // 576 MHz 最低
	primeMaxFreq   = 4608000  // 4.6 GHz
	primeBaseFreq  = 3456000  // 3.456 GHz 默认
	perfMinFreq    = 576000   // 576 MHz
	perfMaxFreq    = 3620000  // 3.62 GHz
	perfBaseFreq   = 2496000  // 2.496 GHz 默认
	primeCoreStart = 0
	primeCoreEnd   = 2
)

func main() {
	resourceLimit := flag.Int("cpu", 8, "模拟的 CPU 核心数量（来自 resources.limits）")
	root := flag.String("root", "/tmp/cpu-sim", "模拟 sysfs/proc 的根目录")
	interval := flag.Duration("interval", 2*time.Second, "动态数据更新间隔")
	flag.Parse()

	if *resourceLimit <= 0 || *resourceLimit > 256 {
		fmt.Fprintf(os.Stderr, "invalid -cpu %d, must be 1-256\n", *resourceLimit)
		os.Exit(1)
	}

	nprocVal := getNproc()
	numCPU := *resourceLimit
	if nprocVal < numCPU {
		numCPU = nprocVal
	}
	fmt.Printf("cpu-simulator: resources.limit=%d, nproc=%d, using min=%d\n", *resourceLimit, nprocVal, numCPU)

	if err := initSoC(*root, numCPU); err != nil {
		fmt.Fprintf(os.Stderr, "init SoC failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("cpu-simulator: Snapdragon 8 Elite Gen 5, %d CPUs, root=%s, interval=%v\n", numCPU, *root, *interval)
	fmt.Println("Press Ctrl+C to stop.")

	runDynamicUpdates(*root, numCPU, *interval)
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func initSoC(root string, numCPU int) error {
	sysCPU := filepath.Join(root, "sys", "devices", "system", "cpu")
	if err := os.MkdirAll(sysCPU, 0755); err != nil {
		return err
	}

	// 全局 CPU 拓扑文件
	writeFile(filepath.Join(sysCPU, "kernel_max"), strconv.Itoa(numCPU-1)+"\n")
	writeFile(filepath.Join(sysCPU, "offline"), "\n")
	online := "0"
	if numCPU > 1 {
		online = fmt.Sprintf("0-%d\n", numCPU-1)
	} else {
		online += "\n"
	}
	writeFile(filepath.Join(sysCPU, "online"), online)
	writeFile(filepath.Join(sysCPU, "possible"), online)
	writeFile(filepath.Join(sysCPU, "present"), online)
	writeFile(filepath.Join(sysCPU, "probe"), "")
	writeFile(filepath.Join(sysCPU, "release"), "")

	// 系统级 cpufreq: boost, policy0 (Prime), policy1 (Perf)
	writeFile(filepath.Join(sysCPU, "cpufreq", "boost"), "1\n")
	initPolicyDir(sysCPU, numCPU, 0, primeCoreEnd, primeMinFreq, primeMaxFreq, primeBaseFreq)
	if numCPU > primeCoreEnd {
		initPolicyDir(sysCPU, numCPU, primeCoreEnd, numCPU, perfMinFreq, perfMaxFreq, perfBaseFreq)
	}

	// 系统级 cpuidle
	writeFile(filepath.Join(sysCPU, "cpuidle", "available_governors"), "ladder teo menu haltpoll\n")
	writeFile(filepath.Join(sysCPU, "cpuidle", "current_driver"), "qcom_idle\n")
	writeFile(filepath.Join(sysCPU, "cpuidle", "current_governor"), "menu\n")
	writeFile(filepath.Join(sysCPU, "cpuidle", "current_governor_ro"), "menu\n")

	// 每个 CPU 的 topology、cpufreq、stats
	for i := 0; i < numCPU; i++ {
		minFreq, maxFreq, baseFreq := getFreqRange(i, numCPU)
		freqs := getAvailableFrequencies(minFreq, maxFreq)

		// topology: 骁龙 2+6，Prime cluster 0-1，Perf cluster 2-7
		topoDir := filepath.Join(sysCPU, fmt.Sprintf("cpu%d", i), "topology")
		coreID := i
		physicalID := 0
		if i < primeCoreEnd {
			physicalID = 0 // Prime cluster
		} else {
			physicalID = 1 // Perf cluster
		}
		writeFile(filepath.Join(topoDir, "core_id"), strconv.Itoa(coreID)+"\n")
		writeFile(filepath.Join(topoDir, "physical_package_id"), strconv.Itoa(physicalID)+"\n")
		// core_siblings: 同 cluster 内所有 CPU (hex mask)
		if i < primeCoreEnd {
			writeFile(filepath.Join(topoDir, "core_siblings_list"), "0-1\n")
			writeFile(filepath.Join(topoDir, "core_siblings"), "00000003\n")
		} else {
			writeFile(filepath.Join(topoDir, "core_siblings_list"), fmt.Sprintf("2-%d\n", numCPU-1))
			mask := (1<<uint(numCPU) - 1) - ((1 << uint(primeCoreEnd)) - 1)
			writeFile(filepath.Join(topoDir, "core_siblings"), fmt.Sprintf("%08x\n", mask))
		}
		// thread_siblings: 无 SMT，每核单线程
		writeFile(filepath.Join(topoDir, "thread_siblings_list"), strconv.Itoa(i)+"\n")
		writeFile(filepath.Join(topoDir, "thread_siblings"), fmt.Sprintf("%08x\n", 1<<uint(i)))

		// cpufreq
		cpufreqDir := filepath.Join(sysCPU, fmt.Sprintf("cpu%d", i), "cpufreq")
		writeFile(filepath.Join(cpufreqDir, "scaling_min_freq"), strconv.Itoa(minFreq)+"\n")
		writeFile(filepath.Join(cpufreqDir, "scaling_max_freq"), strconv.Itoa(maxFreq)+"\n")
		writeFile(filepath.Join(cpufreqDir, "scaling_cur_freq"), strconv.Itoa(baseFreq)+"\n")
		writeFile(filepath.Join(cpufreqDir, "cpuinfo_min_freq"), strconv.Itoa(minFreq)+"\n")
		writeFile(filepath.Join(cpufreqDir, "cpuinfo_max_freq"), strconv.Itoa(maxFreq)+"\n")
		writeFile(filepath.Join(cpufreqDir, "cpuinfo_cur_freq"), strconv.Itoa(baseFreq)+"\n")
		writeFile(filepath.Join(cpufreqDir, "scaling_governor"), "schedutil\n")
		writeFile(filepath.Join(cpufreqDir, "related_cpus"), strconv.Itoa(i)+"\n")
		writeFile(filepath.Join(cpufreqDir, "affected_cpus"), strconv.Itoa(i)+"\n")
		writeFile(filepath.Join(cpufreqDir, "scaling_available_frequencies"), strings.TrimSpace(strings.Join(intSliceToStr(freqs), " "))+"\n")
		writeFile(filepath.Join(cpufreqDir, "scaling_available_governors"), "conservative ondemand userspace powersave performance schedutil\n")
		writeFile(filepath.Join(cpufreqDir, "scaling_driver"), "qcom-cpufreq-hw\n")
		writeFile(filepath.Join(cpufreqDir, "bios_limit"), "\n")
		writeFile(filepath.Join(cpufreqDir, "cpuinfo_transition_latency"), "0\n")
		writeFile(filepath.Join(cpufreqDir, "scaling_setspeed"), "<unsupported>\n")
		writeFile(filepath.Join(cpufreqDir, "freqdomain_cpus"), strconv.Itoa(i)+"\n")

		// cpufreq/stats
		statsDir := filepath.Join(cpufreqDir, "stats")
		writeFile(filepath.Join(statsDir, "time_in_state"), buildTimeInState(freqs, baseFreq))
		writeFile(filepath.Join(statsDir, "total_trans"), "0\n")
		writeFile(filepath.Join(statsDir, "trans_table"), buildTransTable(freqs))
		writeFile(filepath.Join(statsDir, "reset"), "")

		// cache - L1i, L1d, L2, L3 (完整 ARM 结构)
		cacheSpecs := []struct {
			name, level, typ, size, lineSize, sets, ways, alloc, write string
		}{
			{"index0", "1", "Instruction", "64", "64", "64", "4", "ReadAllocate", "WriteBack"},
			{"index1", "1", "Data", "64", "64", "64", "4", "ReadWriteAllocate", "WriteBack"},
			{"index2", "2", "Unified", "512", "64", "512", "16", "ReadWriteAllocate", "WriteBack"},
			{"index3", "3", "Unified", "8192", "64", "8192", "16", "ReadWriteAllocate", "WriteBack"},
		}
		for j, c := range cacheSpecs {
			cacheDir := filepath.Join(sysCPU, fmt.Sprintf("cpu%d", i), "cache", c.name)
			writeFile(filepath.Join(cacheDir, "level"), c.level+"\n")
			writeFile(filepath.Join(cacheDir, "type"), c.typ+"\n")
			writeFile(filepath.Join(cacheDir, "size"), c.size+"K\n")
			writeFile(filepath.Join(cacheDir, "coherency_line_size"), c.lineSize+"\n")
			writeFile(filepath.Join(cacheDir, "number_of_sets"), c.sets+"\n")
			writeFile(filepath.Join(cacheDir, "ways_of_associativity"), c.ways+"\n")
			writeFile(filepath.Join(cacheDir, "allocation_policy"), c.alloc+"\n")
			writeFile(filepath.Join(cacheDir, "write_policy"), c.write+"\n")
			writeFile(filepath.Join(cacheDir, "physical_line_partition"), "1\n")
			writeFile(filepath.Join(cacheDir, "id"), strconv.Itoa(i*4+j)+"\n")
			if c.level == "3" {
				sharedList := "0"
				if numCPU > 1 {
					sharedList = fmt.Sprintf("0-%d", numCPU-1)
				}
				writeFile(filepath.Join(cacheDir, "shared_cpu_list"), sharedList+"\n")
				writeFile(filepath.Join(cacheDir, "shared_cpu_map"), fmt.Sprintf("%0*x\n", (numCPU+3)/4, (1<<uint(numCPU))-1))
			} else {
				writeFile(filepath.Join(cacheDir, "shared_cpu_list"), strconv.Itoa(i)+"\n")
				writeFile(filepath.Join(cacheDir, "shared_cpu_map"), fmt.Sprintf("%08x\n", 1<<uint(i)))
			}
		}

		// cpuidle (完整 state 属性: state0 POLL, state1 WFI, state2 cpu-sleep)
		cpuidleDir := filepath.Join(sysCPU, fmt.Sprintf("cpu%d", i), "cpuidle")
		cpuidleStates := []struct {
			name, desc, latency, residency, power string
		}{
			{"POLL", "CPUIDLE CORE POLL IDLE", "0", "0", "0"},
			{"WFI", "ARM WFI", "1", "1", "0"},
			{"cpu-sleep-0", "CPU sleep 0", "110", "3000", "0"},
		}
		for si, s := range cpuidleStates {
			stateDir := filepath.Join(cpuidleDir, fmt.Sprintf("state%d", si))
			writeFile(filepath.Join(stateDir, "name"), s.name+"\n")
			writeFile(filepath.Join(stateDir, "desc"), s.desc+"\n")
			writeFile(filepath.Join(stateDir, "latency"), s.latency+"\n")
			writeFile(filepath.Join(stateDir, "residency"), s.residency+"\n")
			writeFile(filepath.Join(stateDir, "power"), s.power+"\n")
			writeFile(filepath.Join(stateDir, "usage"), "0\n")
			writeFile(filepath.Join(stateDir, "time"), "0\n")
			writeFile(filepath.Join(stateDir, "above"), "0\n")
			writeFile(filepath.Join(stateDir, "below"), "0\n")
			writeFile(filepath.Join(stateDir, "disable"), "0\n")
			writeFile(filepath.Join(stateDir, "default_status"), "enabled\n")
		}

		// topology 扩展: die_id, cluster_id (ARM)
		writeFile(filepath.Join(topoDir, "die_id"), "0\n")
		writeFile(filepath.Join(topoDir, "cluster_id"), strconv.Itoa(physicalID)+"\n")
		writeFile(filepath.Join(topoDir, "cluster_cpus_list"), func() string {
			if i < primeCoreEnd {
				return "0-1\n"
			}
			return fmt.Sprintf("2-%d\n", numCPU-1)
		}())
		writeFile(filepath.Join(topoDir, "cluster_cpus"), func() string {
			if i < primeCoreEnd {
				return "00000003\n"
			}
			mask := (1<<uint(numCPU) - 1) - ((1 << uint(primeCoreEnd)) - 1)
			return fmt.Sprintf("%08x\n", mask)
		}())
	}

	// 系统级 cpufreq/cpuidle (Kbox 风格: cpufreq/cpuidle 在 system/cpu 下)
	sysCpuidleDir := filepath.Join(sysCPU, "cpufreq", "cpuidle")
	writeFile(filepath.Join(sysCpuidleDir, "driver", "name"), "qcom_idle\n")
	for si, s := range []struct {
		name, desc, latency, residency string
	}{
		{"WFI", "ARM WFI", "1", "1"},
		{"cpu-sleep-0", "CPU sleep 0", "110", "3000"},
	} {
		stateDir := filepath.Join(sysCpuidleDir, fmt.Sprintf("state%d", si))
		writeFile(filepath.Join(stateDir, "name"), s.name+"\n")
		writeFile(filepath.Join(stateDir, "desc"), s.desc+"\n")
		writeFile(filepath.Join(stateDir, "latency"), s.latency+"\n")
		writeFile(filepath.Join(stateDir, "residency"), s.residency+"\n")
		writeFile(filepath.Join(stateDir, "usage"), "0\n")
		writeFile(filepath.Join(stateDir, "time"), "0\n")
		writeFile(filepath.Join(stateDir, "disable"), "0\n")
	}

	// thermal zone - Qualcomm 风格
	thermalDir := filepath.Join(root, "sys", "class", "thermal", "thermal_zone0")
	writeFile(filepath.Join(thermalDir, "type"), "cpu-thermal\n")
	writeFile(filepath.Join(thermalDir, "temp"), "45000\n")
	writeFile(filepath.Join(thermalDir, "mode"), "enabled\n")
	writeFile(filepath.Join(thermalDir, "policy"), "step_wise\n")
	writeFile(filepath.Join(thermalDir, "trip_point_0_temp"), "95000\n")
	writeFile(filepath.Join(thermalDir, "trip_point_0_type"), "critical\n")

	// thermal_zone1 - GPU 等
	thermalDir1 := filepath.Join(root, "sys", "class", "thermal", "thermal_zone1")
	writeFile(filepath.Join(thermalDir1, "type"), "gpu-thermal\n")
	writeFile(filepath.Join(thermalDir1, "temp"), "42000\n")
	writeFile(filepath.Join(thermalDir1, "mode"), "enabled\n")
	writeFile(filepath.Join(thermalDir1, "policy"), "step_wise\n")

	// thermal_zone2 - battery
	thermalDir2 := filepath.Join(root, "sys", "class", "thermal", "thermal_zone2")
	writeFile(filepath.Join(thermalDir2, "type"), "battery\n")
	writeFile(filepath.Join(thermalDir2, "temp"), "32000\n")
	writeFile(filepath.Join(thermalDir2, "mode"), "enabled\n")
	writeFile(filepath.Join(thermalDir2, "policy"), "step_wise\n")

	// thermal_zone3 - modem
	thermalDir3 := filepath.Join(root, "sys", "class", "thermal", "thermal_zone3")
	writeFile(filepath.Join(thermalDir3, "type"), "modem-thermal\n")
	writeFile(filepath.Join(thermalDir3, "temp"), "38000\n")
	writeFile(filepath.Join(thermalDir3, "mode"), "enabled\n")

	// cooling_device (可选)
	coolingDir := filepath.Join(root, "sys", "class", "thermal", "cooling_device0")
	writeFile(filepath.Join(coolingDir, "type"), "Processor\n")
	writeFile(filepath.Join(coolingDir, "max_state"), "10\n")
	writeFile(filepath.Join(coolingDir, "cur_state"), "0\n")

	return nil
}

// initPolicyDir 创建系统级 cpufreq/policyN 目录 (Android/Kbox 风格)
func initPolicyDir(sysCPU string, numCPU, start, end int, minFreq, maxFreq, baseFreq int) {
	policyID := 0
	if start >= primeCoreEnd {
		policyID = 1
	}
	policyDir := filepath.Join(sysCPU, "cpufreq", fmt.Sprintf("policy%d", policyID))
	affected := ""
	for i := start; i < end; i++ {
		if affected != "" {
			affected += " "
		}
		affected += strconv.Itoa(i)
	}
	freqs := getAvailableFrequencies(minFreq, maxFreq)
	writeFile(filepath.Join(policyDir, "affected_cpus"), affected+"\n")
	writeFile(filepath.Join(policyDir, "related_cpus"), affected+"\n")
	writeFile(filepath.Join(policyDir, "cpuinfo_min_freq"), strconv.Itoa(minFreq)+"\n")
	writeFile(filepath.Join(policyDir, "cpuinfo_max_freq"), strconv.Itoa(maxFreq)+"\n")
	writeFile(filepath.Join(policyDir, "cpuinfo_cur_freq"), strconv.Itoa(baseFreq)+"\n")
	writeFile(filepath.Join(policyDir, "cpuinfo_transition_latency"), "0\n")
	writeFile(filepath.Join(policyDir, "scaling_min_freq"), strconv.Itoa(minFreq)+"\n")
	writeFile(filepath.Join(policyDir, "scaling_max_freq"), strconv.Itoa(maxFreq)+"\n")
	writeFile(filepath.Join(policyDir, "scaling_cur_freq"), strconv.Itoa(baseFreq)+"\n")
	writeFile(filepath.Join(policyDir, "scaling_governor"), "schedutil\n")
	writeFile(filepath.Join(policyDir, "scaling_driver"), "qcom-cpufreq-hw\n")
	writeFile(filepath.Join(policyDir, "scaling_setspeed"), "<unsupported>\n")
	writeFile(filepath.Join(policyDir, "scaling_available_frequencies"), strings.TrimSpace(strings.Join(intSliceToStr(freqs), " "))+"\n")
	writeFile(filepath.Join(policyDir, "scaling_available_governors"), "conservative ondemand userspace powersave performance schedutil interactive\n")
}

func getFreqRange(cpuIdx, numCPU int) (min, max, base int) {
	if cpuIdx < primeCoreEnd {
		return primeMinFreq, primeMaxFreq, primeBaseFreq
	}
	return perfMinFreq, perfMaxFreq, perfBaseFreq
}

func getAvailableFrequencies(minFreq, maxFreq int) []int {
	// 模拟常见步进
	steps := []int{576000, 691200, 806400, 921600, 1036800, 1152000, 1267200, 1382400, 1497600, 1612800, 1728000, 1843200, 1958400, 2073600, 2188800, 2304000, 2419200, 2534400, 2649600, 2764800, 2880000, 2995200, 3110400, 3225600, 3340800, 3456000, 3571200, 3686400, 3801600, 3916800, 4032000, 4147200, 4262400, 4377600, 4492800, 4608000}
	var freqs []int
	for _, s := range steps {
		if s >= minFreq && s <= maxFreq {
			freqs = append(freqs, s)
		}
	}
	if len(freqs) == 0 {
		freqs = []int{minFreq, (minFreq+maxFreq)/2, maxFreq}
	}
	return freqs
}

func intSliceToStr(a []int) []string {
	s := make([]string, len(a))
	for i, v := range a {
		s[i] = strconv.Itoa(v)
	}
	return s
}

func buildTimeInState(freqs []int, curFreq int) string {
	var sb strings.Builder
	for _, f := range freqs {
		time := 0
		if f == curFreq {
			time = 1000 // 模拟已运行一段时间
		}
		sb.WriteString(fmt.Sprintf("%d %d\n", f, time))
	}
	return sb.String()
}

func buildTransTable(freqs []int) string {
	var sb strings.Builder
	sb.WriteString("   From  :    To\n")
	sb.WriteString("         :")
	for _, f := range freqs {
		sb.WriteString(fmt.Sprintf(" %7d", f))
	}
	sb.WriteString("\n")
	for _, f := range freqs {
		sb.WriteString(fmt.Sprintf("%7d:", f))
		for range freqs {
			sb.WriteString("       0")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// 动态更新频率和温度，模拟负载变化
func runDynamicUpdates(root string, numCPU int, interval time.Duration) {
	rand.Seed(time.Now().UnixNano())

	freqs := make([]int, numCPU)
	for i := range freqs {
		_, _, base := getFreqRange(i, numCPU)
		freqs[i] = base
	}

	timeInState := make([][]int, numCPU)
	for i := 0; i < numCPU; i++ {
		minF, maxF, _ := getFreqRange(i, numCPU)
		freqList := getAvailableFrequencies(minF, maxF)
		timeInState[i] = make([]int, len(freqList))
	}

	totalTrans := make([]int, numCPU)
	temp := 45000
	tick := 0

	for range time.NewTicker(interval).C {
		tick++

		for i := 0; i < numCPU; i++ {
			minF, maxF, _ := getFreqRange(i, numCPU)
			delta := (rand.Intn(3) - 1) * 100000
			freqs[i] += delta
			if freqs[i] < minF {
				freqs[i] = minF
			}
			if freqs[i] > maxF {
				freqs[i] = maxF
			}

			cpufreqDir := filepath.Join(root, "sys", "devices", "system", "cpu",
				fmt.Sprintf("cpu%d", i), "cpufreq")
			writeFile(filepath.Join(cpufreqDir, "scaling_cur_freq"), strconv.Itoa(freqs[i])+"\n")
			writeFile(filepath.Join(cpufreqDir, "cpuinfo_cur_freq"), strconv.Itoa(freqs[i])+"\n")
		}

		// 更新 policy0/policy1 scaling_cur_freq (取 cluster 内首个 CPU 频率)
		if numCPU > 0 {
			writeFile(filepath.Join(root, "sys", "devices", "system", "cpu", "cpufreq", "policy0", "scaling_cur_freq"), strconv.Itoa(freqs[0])+"\n")
			writeFile(filepath.Join(root, "sys", "devices", "system", "cpu", "cpufreq", "policy0", "cpuinfo_cur_freq"), strconv.Itoa(freqs[0])+"\n")
		}
		if numCPU > primeCoreEnd {
			writeFile(filepath.Join(root, "sys", "devices", "system", "cpu", "cpufreq", "policy1", "scaling_cur_freq"), strconv.Itoa(freqs[primeCoreEnd])+"\n")
			writeFile(filepath.Join(root, "sys", "devices", "system", "cpu", "cpufreq", "policy1", "cpuinfo_cur_freq"), strconv.Itoa(freqs[primeCoreEnd])+"\n")
		}

		for i := 0; i < numCPU; i++ {
			minF, maxF, _ := getFreqRange(i, numCPU)
			cpufreqDir := filepath.Join(root, "sys", "devices", "system", "cpu", fmt.Sprintf("cpu%d", i), "cpufreq")

			// 更新 time_in_state 和 total_trans
			freqList := getAvailableFrequencies(minF, maxF)
			idx := -1
			for j, f := range freqList {
				if f == freqs[i] {
					idx = j
					break
				}
			}
			if idx >= 0 && idx < len(timeInState[i]) {
				timeInState[i][idx] += 10 // 每 tick 增加 10 单位 (100ms)
				totalTrans[i]++
			}

			// 写回 stats
			statsDir := filepath.Join(cpufreqDir, "stats")
			var sb strings.Builder
			for j, f := range freqList {
				if j < len(timeInState[i]) {
					sb.WriteString(fmt.Sprintf("%d %d\n", f, timeInState[i][j]))
				}
			}
			writeFile(filepath.Join(statsDir, "time_in_state"), sb.String())
			writeFile(filepath.Join(statsDir, "total_trans"), strconv.Itoa(totalTrans[i])+"\n")
		}

		avgFreq := 0
		for _, f := range freqs {
			avgFreq += f
		}
		avgFreq /= numCPU
		baseTemp := 40000 + (avgFreq-576000)/40000*1000
		wave := int(5000 * math.Sin(float64(tick)*0.1))
		temp = baseTemp + wave
		if temp < 35000 {
			temp = 35000
		}
		if temp > 95000 {
			temp = 95000
		}

		writeFile(filepath.Join(root, "sys", "class", "thermal", "thermal_zone0", "temp"), strconv.Itoa(temp)+"\n")
		writeFile(filepath.Join(root, "sys", "class", "thermal", "thermal_zone1", "temp"), strconv.Itoa(temp-3000)+"\n")
		writeFile(filepath.Join(root, "sys", "class", "thermal", "thermal_zone2", "temp"), strconv.Itoa(32000+temp/20)+"\n")
		writeFile(filepath.Join(root, "sys", "class", "thermal", "thermal_zone3", "temp"), strconv.Itoa(temp-5000)+"\n")
		// cooling_device cur_state 随温度变化 (0-10)
		coolState := (temp - 35000) / 6000
		if coolState < 0 {
			coolState = 0
		}
		if coolState > 10 {
			coolState = 10
		}
		writeFile(filepath.Join(root, "sys", "class", "thermal", "cooling_device0", "cur_state"), strconv.Itoa(coolState)+"\n")
	}
}
