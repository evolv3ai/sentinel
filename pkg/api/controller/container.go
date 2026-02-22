package controller

import (
	"fmt"
	"io"
	"log"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coollabsio/sentinel/pkg/json"
	"github.com/coollabsio/sentinel/pkg/types"
	dockerTypes "github.com/docker/docker/api/types"
	"github.com/gin-gonic/gin"
)

var containerIdRegex = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func (c *Controller) setupContainerRoutes() {
	// Live container list from Docker daemon
	c.ginE.GET("/api/containers", func(ctx *gin.Context) {
		incomingToken := ctx.GetHeader("Authorization")
		if incomingToken != "Bearer "+c.config.Token {
			ctx.JSON(401, gin.H{"error": "Unauthorized"})
			return
		}

		resp, err := c.dockerClient.MakeRequest("/containers/json?all=true")
		if err != nil {
			ctx.JSON(500, gin.H{"error": fmt.Sprintf("docker api error: %v", err)})
			return
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			ctx.JSON(500, gin.H{"error": fmt.Sprintf("read error: %v", err)})
			return
		}

		var containers []dockerTypes.Container
		if err := json.Unmarshal(body, &containers); err != nil {
			ctx.JSON(500, gin.H{"error": fmt.Sprintf("unmarshal error: %v", err)})
			return
		}

		var result []types.Container
		for _, container := range containers {
			name := ""
			if len(container.Names) > 0 && len(container.Names[0]) > 1 {
				name = container.Names[0][1:]
			} else if len(container.Names) > 0 {
				name = container.Names[0]
			} else {
				name = container.ID[:12]
			}

			// Get health status via inspect
			healthStatus := "unknown"
			inspResp, err := c.dockerClient.MakeRequest(fmt.Sprintf("/containers/%s/json", container.ID))
			if err == nil {
				defer inspResp.Body.Close()
				inspBody, err := io.ReadAll(inspResp.Body)
				if err == nil {
					var inspectData dockerTypes.ContainerJSON
					if err := json.Unmarshal(inspBody, &inspectData); err == nil {
						if inspectData.State != nil && inspectData.State.Health != nil {
							healthStatus = inspectData.State.Health.Status
						}
					}
				}
			}

			result = append(result, types.Container{
				Time:         time.Now().Format("2006-01-02T15:04:05Z"),
				ID:           container.ID,
				Image:        container.Image,
				Name:         name,
				State:        container.State,
				Labels:       container.Labels,
				HealthStatus: healthStatus,
			})
		}
		ctx.JSON(200, result)
	})

	// Disk usage endpoint
	c.ginE.GET("/api/disk", func(ctx *gin.Context) {
		incomingToken := ctx.GetHeader("Authorization")
		if incomingToken != "Bearer "+c.config.Token {
			ctx.JSON(401, gin.H{"error": "Unauthorized"})
			return
		}

		fs := syscall.Statfs_t{}
		if err := syscall.Statfs("/", &fs); err != nil {
			ctx.JSON(500, gin.H{"error": fmt.Sprintf("statfs error: %v", err)})
			return
		}

		total := fs.Blocks * uint64(fs.Bsize)
		free := fs.Bfree * uint64(fs.Bsize)
		used := total - free
		pct := float64(used) / float64(total) * 100

		ctx.JSON(200, gin.H{
			"total":       total,
			"used":        used,
			"free":        free,
			"usedPercent": pct,
		})
	})
	c.ginE.GET("/api/container/:containerId/cpu/history", func(ctx *gin.Context) {
		containerID := strings.ReplaceAll(ctx.Param("containerId"), "/", "")
		containerID = containerIdRegex.ReplaceAllString(containerID, "")
		from := ctx.Query("from")
		if from == "" {
			from = "1970-01-01T00:00:01Z"
		}
		to := ctx.Query("to")
		if to == "" {
			to = time.Now().UTC().Format("2006-01-02T15:04:05Z")
		}

		// Validate date format
		layout := "2006-01-02T15:04:05Z"
		if from != "" {
			if _, err := time.Parse(layout, from); err != nil {
				ctx.JSON(400, gin.H{"error": "Invalid 'from' date format. Use YYYY-MM-DDTHH:MM:SSZ"})
				return
			}
		}
		if to != "" {
			if _, err := time.Parse(layout, to); err != nil {
				ctx.JSON(400, gin.H{"error": "Invalid 'to' date format. Use YYYY-MM-DDTHH:MM:SSZ"})
				return
			}
		}

		var params []interface{}
		query := "SELECT time, container_id, percent FROM container_cpu_usage WHERE container_id = ?"
		params = append(params, containerID)
		if from != "" {
			fromTime, _ := time.Parse(layout, from)
			query += " AND CAST(time AS BIGINT) >= ?"
			params = append(params, fromTime.UnixMilli())
		}
		if to != "" {
			toTime, _ := time.Parse(layout, to)
			if from != "" {
				query += " AND"
			} else {
				query += " WHERE"
			}
			query += " CAST(time AS BIGINT) <= ?"
			params = append(params, toTime.UnixMilli())
		}
		query += " ORDER BY CAST(time AS BIGINT) ASC"
		rows, err := c.database.Query(query, params...)
		if err != nil {
			ctx.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()

		usages := []CpuUsage{}
		for rows.Next() {
			var usage CpuUsage
			var containerID string
			if err := rows.Scan(&usage.Time, &containerID, &usage.Percent); err != nil {
				ctx.JSON(500, gin.H{"error": err.Error()})
				return
			}
			timeInt, _ := strconv.ParseInt(usage.Time, 10, 64)
			if gin.Mode() == gin.DebugMode {
				usage.HumanFriendlyTime = time.UnixMilli(timeInt).Format(layout)
			}
			usages = append(usages, usage)
		}
		ctx.JSON(200, usages)
	})
	c.ginE.GET("/api/container/:containerId/memory/history", func(ctx *gin.Context) {
		containerID := strings.ReplaceAll(ctx.Param("containerId"), "/", "")
		containerID = containerIdRegex.ReplaceAllString(containerID, "")
		from := ctx.Query("from")
		if from == "" {
			from = "1970-01-01T00:00:01Z"
		}
		to := ctx.Query("to")
		if to == "" {
			to = time.Now().UTC().Format("2006-01-02T15:04:05Z")
		}

		if c.config.Debug {
			log.Printf("[DEBUG] Container memory history request - containerID: %s, from: %s, to: %s", containerID, from, to)
		}

		// Validate date format
		layout := "2006-01-02T15:04:05Z"
		if from != "" {
			if _, err := time.Parse(layout, from); err != nil {
				ctx.JSON(400, gin.H{"error": "Invalid 'from' date format. Use YYYY-MM-DDTHH:MM:SSZ"})
				return
			}
		}
		if to != "" {
			if _, err := time.Parse(layout, to); err != nil {
				ctx.JSON(400, gin.H{"error": "Invalid 'to' date format. Use YYYY-MM-DDTHH:MM:SSZ"})
				return
			}
		}

		var params []interface{}
		query := "SELECT time, container_id, total, available, used, usedPercent, free FROM container_memory_usage WHERE container_id = ?"
		params = append(params, containerID)
		if from != "" {
			fromTime, _ := time.Parse(layout, from)
			query += " AND CAST(time AS BIGINT) >= ?"
			params = append(params, fromTime.UnixMilli())
		}
		if to != "" {
			toTime, _ := time.Parse(layout, to)
			if from != "" {
				query += " AND"
			} else {
				query += " WHERE"
			}
			query += " CAST(time AS BIGINT) <= ?"
			params = append(params, toTime.UnixMilli())
		}
		query += " ORDER BY CAST(time AS BIGINT) ASC"

		if c.config.Debug {
			log.Printf("[DEBUG] Container memory query: %s with params: %v", query, params)
		}

		rows, err := c.database.Query(query, params...)
		if err != nil {
			ctx.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()

		usages := []MemUsage{}
		rowCount := 0
		for rows.Next() {
			var usage MemUsage
			var containerID string
			var totalStr, availableStr, usedStr, usedPercentStr, freeStr string
			if err := rows.Scan(&usage.Time, &containerID, &totalStr, &availableStr, &usedStr, &usedPercentStr, &freeStr); err != nil {
				log.Printf("[ERROR] Container scan failed: %v", err)
				ctx.JSON(500, gin.H{"error": err.Error()})
				return
			}
			rowCount++
			if c.config.Debug {
				log.Printf("[DEBUG] Container row %d - time: %s, id: %s, total: %s, available: %s, used: %s, usedPercent: %s, free: %s",
					rowCount, usage.Time, containerID, totalStr, availableStr, usedStr, usedPercentStr, freeStr)
			}
			usage.Total, _ = strconv.ParseUint(totalStr, 10, 64)
			usage.Available, _ = strconv.ParseUint(availableStr, 10, 64)
			usage.Used, _ = strconv.ParseUint(usedStr, 10, 64)
			usage.UsedPercent, _ = strconv.ParseFloat(usedPercentStr, 64)
			usage.Free, _ = strconv.ParseUint(freeStr, 10, 64)
			timeInt, _ := strconv.ParseInt(usage.Time, 10, 64)
			if gin.Mode() == gin.DebugMode {
				usage.HumanFriendlyTime = time.UnixMilli(timeInt).Format(layout)
			}
			usages = append(usages, usage)
		}
		if c.config.Debug {
			log.Printf("[DEBUG] Returning %d container memory records", len(usages))
		}
		ctx.JSON(200, usages)
	})
}
