package beat

import (
	"github.com/elastic/beats/libbeat/common"
	"github.com/fsouza/go-dockerclient"
	"strings"
	"time"
)

type EventGenerator struct {
	networkStats      map[string]map[string]NetworkData
	blkioStats        map[string]BlkioData
	calculatorFactory CalculatorFactory
	period            time.Duration
}

func (d *EventGenerator) getContainerEvent(container *docker.APIContainers, stats *docker.Stats) common.MapStr {
	event := common.MapStr{
		"@timestamp":    common.Time(stats.Read),
		"type":          "container",
		"containerID":   container.ID,
		"containerName": d.extractContainerName(container.Names),
		"container": common.MapStr{
			"id":         container.ID,
			"command":    container.Command,
			"created":    time.Unix(container.Created, 0),
			"image":      container.Image,
			"labels":     container.Labels,
			"names":      container.Names,
			"ports":      d.convertContainerPorts(&container.Ports),
			"sizeRootFs": container.SizeRootFs,
			"sizeRw":     container.SizeRw,
			"status":     container.Status,
		},
	}
	return event
}

func (d *EventGenerator) getCpuEvent(container *docker.APIContainers, stats *docker.Stats) common.MapStr {

	calculator := d.calculatorFactory.newCPUCalculator(
		CPUData{
			perCpuUsage:       stats.PreCPUStats.CPUUsage.PercpuUsage,
			totalUsage:        stats.PreCPUStats.CPUUsage.TotalUsage,
			usageInKernelmode: stats.PreCPUStats.CPUUsage.UsageInKernelmode,
			usageInUsermode:   stats.PreCPUStats.CPUUsage.UsageInUsermode,
		},
		CPUData{
			perCpuUsage:       stats.CPUStats.CPUUsage.PercpuUsage,
			totalUsage:        stats.CPUStats.CPUUsage.TotalUsage,
			usageInKernelmode: stats.CPUStats.CPUUsage.UsageInKernelmode,
			usageInUsermode:   stats.CPUStats.CPUUsage.UsageInUsermode,
		},
	)

	event := common.MapStr{
		"@timestamp":    common.Time(stats.Read),
		"type":          "cpu",
		"containerID":   container.ID,
		"containerName": d.extractContainerName(container.Names),
		"cpu": common.MapStr{
			"percpuUsage":       calculator.perCpuUsage(),
			"totalUsage":        calculator.totalUsage(),
			"usageInKernelmode": calculator.usageInKernelmode(),
			"usageInUsermode":   calculator.usageInUsermode(),
		},
	}

	return event
}

func (d *EventGenerator) getNetworksEvent(container *docker.APIContainers, stats *docker.Stats) []common.MapStr {
	events := []common.MapStr{}

	for netName, netStats := range stats.Networks {
		events = append(events, d.getNetworkEvent(container, stats.Read, netName, &netStats))
	}

	// purge old saved data
	for container, networkDataMap := range d.networkStats {
		useless := true
		for networkName, networkData := range networkDataMap {
			// if data older than two ticks, then delete it
			if d.expiredSavedData(networkData.time) {
				delete(networkDataMap, networkName)
			} else {
				useless = false
			}
		}

		// if all network data are useless, then delete container entry
		if useless {
			delete(d.networkStats, container)
		}
	}

	return events
}

func (d *EventGenerator) getNetworkEvent(container *docker.APIContainers, time time.Time, network string, networkStats *docker.NetworkStats) common.MapStr {

	newNetworkData := NetworkData{
		time:      time,
		rxBytes:   networkStats.RxBytes,
		rxDropped: networkStats.RxDropped,
		rxErrors:  networkStats.RxErrors,
		rxPackets: networkStats.RxPackets,
		txBytes:   networkStats.TxBytes,
		txDropped: networkStats.TxDropped,
		txErrors:  networkStats.TxErrors,
		txPackets: networkStats.TxPackets,
	}

	var event common.MapStr

	oldNetworkData, ok := d.networkStats[container.ID][network]

	if ok {
		calculator := d.calculatorFactory.newNetworkCalculator(oldNetworkData, newNetworkData)
		event = common.MapStr{
			"@timestamp":    common.Time(time),
			"type":          "net",
			"containerID":   container.ID,
			"containerName": d.extractContainerName(container.Names),
			"net": common.MapStr{
				"name":         network,
				"rxBytes_ps":   calculator.getRxBytesPerSecond(),
				"rxDropped_ps": calculator.getRxDroppedPerSecond(),
				"rxErrors_ps":  calculator.getRxErrorsPerSecond(),
				"rxPackets_ps": calculator.getRxPacketsPerSecond(),
				"txBytes_ps":   calculator.getTxBytesPerSecond(),
				"txDropped_ps": calculator.getTxDroppedPerSecond(),
				"txErrors_ps":  calculator.getTxErrorsPerSecond(),
				"txPackets_ps": calculator.getTxPacketsPerSecond(),
			},
		}
	} else {
		event = common.MapStr{
			"@timestamp":    common.Time(time),
			"type":          "net",
			"containerID":   container.ID,
			"containerName": d.extractContainerName(container.Names),
			"net": common.MapStr{
				"name":         network,
				"rxBytes_ps":   0,
				"rxDropped_ps": 0,
				"rxErrors_ps":  0,
				"rxPackets_ps": 0,
				"txBytes_ps":   0,
				"txDropped_ps": 0,
				"txErrors_ps":  0,
				"txPackets_ps": 0,
			},
		}
	}

	// save status
	if _, exists := d.networkStats[container.ID]; !exists {
		d.networkStats[container.ID] = map[string]NetworkData{}
	}
	d.networkStats[container.ID][network] = newNetworkData
	return event
}

func (d *EventGenerator) getMemoryEvent(container *docker.APIContainers, stats *docker.Stats) common.MapStr {
	event := common.MapStr{
		"@timestamp":    common.Time(stats.Read),
		"type":          "memory",
		"containerID":   container.ID,
		"containerName": d.extractContainerName(container.Names),
		"memory": common.MapStr{
			"failcnt":  stats.MemoryStats.Failcnt,
			"limit":    stats.MemoryStats.Limit,
			"maxUsage": stats.MemoryStats.MaxUsage,
			"usage":    stats.MemoryStats.Usage,
			"usage_p":  float64(stats.MemoryStats.Usage) / float64(stats.MemoryStats.Limit),
		},
	}

	return event
}

func (d *EventGenerator) getBlkioEvent(container *docker.APIContainers, stats *docker.Stats) common.MapStr {
	blkioStats := d.buildStats(stats.Read, stats.BlkioStats.IOServicedRecursive)

	var event common.MapStr

	oldBlkioStats, ok := d.blkioStats[container.ID]

	if ok {
		calculator := d.calculatorFactory.newBlkioCalculator(oldBlkioStats, blkioStats)
		event = common.MapStr{
			"@timestamp":     common.Time(stats.Read),
			"type":           "blkio",
			"containerID":    container.ID,
			"containerNames": container.Names,
			"blkio": common.MapStr{
				"read":  calculator.getReadPs(),
				"write": calculator.getWritePs(),
				"total": calculator.getTotalPs(),
			},
		}
	} else {
		event = common.MapStr{
			"@timestamp":     common.Time(stats.Read),
			"type":           "blkio",
			"containerID":    container.ID,
			"containerNames": container.Names,
			"blkio": common.MapStr{
				"read":  float64(0),
				"write": float64(0),
				"total": float64(0),
			},
		}
	}

	d.blkioStats[container.ID] = blkioStats

	// purge old saved data
	for containerId, blkioStat := range d.blkioStats {
		// if data older than two ticks, then delete it
		if d.expiredSavedData(blkioStat.time) {
			delete(d.blkioStats, containerId)
		}
	}
	return event
}

func (d *EventGenerator) convertContainerPorts(ports *[]docker.APIPort) []map[string]interface{} {
	var outputPorts []map[string]interface{}
	for _, port := range *ports {
		outputPort := common.MapStr{
			"ip":          port.IP,
			"privatePort": port.PrivatePort,
			"publicPort":  port.PublicPort,
			"type":        port.Type,
		}
		outputPorts = append(outputPorts, outputPort)
	}

	return outputPorts
}

func (d *EventGenerator) cleanOldStats(containers []docker.APIContainers) {
	found := false
	for containerStatKey, _ := range d.networkStats {
		for _, container := range containers {
			if container.ID == containerStatKey {
				found = true
				continue
			}
		}
		if !found {
			delete(d.networkStats, containerStatKey)
		}
	}
}

func (d *EventGenerator) buildStats(time time.Time, entry []docker.BlkioStatsEntry) BlkioData {
	var stats = BlkioData{time, 0, 0, 0}
	for _, s := range entry {
		if s.Op == "Read" {
			stats.reads += s.Value
		} else if s.Op == "Write" {
			stats.writes += s.Value
		} else if s.Op == "Total" {
			stats.totals += s.Value
		}
	}
	return stats
}

func (d *EventGenerator) extractContainerName(names []string) string {
	output := names[0]

	if cap(names) > 1 {
		for _, name := range names {
			if strings.Count(output, "/") > strings.Count(name, "/") {
				output = name
			}
		}
	}
	return strings.Trim(output, "/")
}

func (d *EventGenerator) expiredSavedData(date time.Time) bool {
	return !date.Add(2 * d.period).After(time.Now())
}
