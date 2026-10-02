package service

import (
	"crypto/rand"
	"fmt"
	"nodectl/internal/database"
	"nodectl/internal/logger"

	"gorm.io/gorm"
)

// GenerateRandomNodeName 生成随机节点名称 (node-4位字母数字)
func GenerateRandomNodeName() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		logger.Log.Warn("生成随机节点名称失败，启用回退名称", "error", err)
		return "node-0000" // 容错备用
	}
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return fmt.Sprintf("node-%s", string(b))
}

// AddNode 核心写入节点逻辑
func AddNode(name string, routingType int) (*database.NodePool, error) {
	// 如果用户未输入名称，则自动生成
	if name == "" {
		name = GenerateRandomNodeName()
	}

	node := &database.NodePool{
		Name:        name,
		RoutingType: routingType,             // 1:中转, 2:落地
		Links:       make(map[string]string), // 初始化空的连接 map
	}

	// 写入数据库
	if err := database.DB.Create(node).Error; err != nil {
		logger.Log.Error("服务层异常: 创建节点入库失败", "error", err, "node_name", name)
		return nil, err
	}

	// 立即将新节点添加到通知缓存，确保 Agent WS 连接时可被识别
	AddNodeToNotifyCache(node)

	return node, nil
}

// UpdateNode 更新节点信息 (名称、路由类型、协议链接)
func UpdateNode(uuid string, name string, routingType int, links map[string]string, isBlocked bool, disabledLinks []string, ipv4, ipv6 string) error {
	var node database.NodePool

	if err := database.DB.Where("uuid = ?", uuid).First(&node).Error; err != nil {
		logger.Log.Error("服务层异常: 更新时未找到目标节点", "error", err, "uuid", uuid)
		return err
	}

	node.Name = name
	node.RoutingType = routingType
	node.Links = links
	node.IsBlocked = isBlocked
	node.DisabledLinks = disabledLinks
	node.IPV4 = ipv4
	node.IPV6 = ipv6

	// 解析 Region
	if GlobalGeoIP != nil {
		region := ""
		if ipv4 != "" {
			region = GlobalGeoIP.GetCountryIsoCode(ipv4)
		}
		if region == "" && ipv6 != "" {
			region = GlobalGeoIP.GetCountryIsoCode(ipv6)
		}
		// 如果解析到了，就更新；如果没解析到但IP变空了，可能需要清空 region？
		// 这里策略是：只要解析出有效代码就覆盖，否则保留原样
		if region != "" {
			node.Region = region
			logger.Log.Debug("服务层: 节点 GeoIP 区域自动匹配成功", "uuid", uuid, "region", region)
		}
	}

	if err := database.DB.Save(&node).Error; err != nil {
		logger.Log.Error("服务层异常: 保存节点更新失败", "error", err, "uuid", uuid)
		return err
	}

	logger.Log.Debug("服务层: 节点数据更新成功", "uuid", uuid)
	return nil
}

// ReorderNodes 批量更新节点的路由类型和排序索引
func ReorderNodes(routingType int, uuids []string) error {
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		for index, uuid := range uuids {
			// 更新每个节点的 RoutingType (因为可能从中转拖到了落地)
			// 并更新 SortIndex 为当前数组的下标
			err := tx.Model(&database.NodePool{}).
				Where("uuid = ?", uuid).
				Updates(map[string]interface{}{
					"RoutingType": routingType, // 确保节点归属到新分组
					"SortIndex":   index,       // 更新排序
				}).Error
			if err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		logger.Log.Error("服务层异常: 批量重排节点事务提交失败", "error", err, "routing_type", routingType)
		return err
	}
	return nil
}
