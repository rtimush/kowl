package kafka

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/Shopify/sarama"
	"go.uber.org/zap"
)

// GroupDescription for a Kafka Consumer Group
type GroupDescription struct {
	GroupID      string                    `json:"groupId"`
	State        string                    `json:"state"`
	ProtocolType string                    `json:"protocolType"`
	Protocol     string                    `json:"-"`
	Members      []*GroupMemberDescription `json:"members"`
}

// GroupMemberDescription is a member (e. g. connected host) of a Consumer Group
type GroupMemberDescription struct {
	ID          string                   `json:"id"`
	ClientID    string                   `json:"clientId"`
	ClientHost  string                   `json:"clientHost"`
	Assignments []*GroupMemberAssignment `json:"assignments"`
}

// GroupMemberAssignment represents a partition assignment for a group member
type GroupMemberAssignment struct {
	TopicName    string  `json:"topicName"`
	PartitionIDs []int32 `json:"partitionIds"`
}

// DescribeConsumerGroups fetches additional information from Kafka about one or more consumer groups
func (s *Service) DescribeConsumerGroups(ctx context.Context, groups []string) ([]*GroupDescription, error) {
	// 1. Bucket all groupIDs by their respective consumer group coordinator/broker
	brokersByID := make(map[int32]*sarama.Broker)
	groupsByBrokerID := make(map[int32][]string)
	for _, group := range groups {
		coordinator, err := s.Client.Coordinator(group)
		if err != nil {
			return nil, err
		}

		id := coordinator.ID()
		brokersByID[id] = coordinator
		groupsByBrokerID[id] = append(groupsByBrokerID[id], group)
	}

	// 2. Describe groups in bulk for each broker
	type response struct {
		Err      error
		Groups   []*sarama.GroupDescription
		BrokerID int32
	}
	resCh := make(chan response, len(groupsByBrokerID))
	wg := sync.WaitGroup{}

	for id, groups := range groupsByBrokerID {
		b := brokersByID[id]

		wg.Add(1)
		go func(broker *sarama.Broker, grps []string) {
			defer wg.Done()

			req := &sarama.DescribeGroupsRequest{Groups: grps}
			r, err := broker.DescribeGroups(req)
			if err != nil {
				resCh <- response{
					Err:      err,
					Groups:   nil,
					BrokerID: b.ID(),
				}
				return
			}
			resCh <- response{
				Err:      nil,
				Groups:   r.Groups,
				BrokerID: b.ID(),
			}
		}(b, groups)
	}

	go func() {
		wg.Wait()
		close(resCh)
	}()

	// 3. Fetch all group description responses and convert them so that they match our desired response format
	descriptions := make([]*GroupDescription, 0)
Loop:
	for {
		select {
		case d, ok := <-resCh:
			if !ok {
				// If channel has been closed we're done, so let's exit the loop
				break Loop
			}
			if d.Err != nil {
				return nil, fmt.Errorf("broker with id '%v' failed to describe the consumer groups: %v", d.BrokerID, d.Err)
			}

			converted, err := convertSaramaGroupDescriptions(s.Logger, d.Groups)
			if err != nil {
				return nil, err
			}
			descriptions = append(descriptions, converted...)
		case <-ctx.Done():
			s.Logger.Error("context has been cancelled", zap.String("method", "list_consumer_groups"))
			return nil, fmt.Errorf("context has been cancelled")
		}
	}

	sort.Slice(descriptions, func(i, j int) bool { return descriptions[i].GroupID < descriptions[j].GroupID })

	return descriptions, nil
}

func convertSaramaGroupDescriptions(logger *zap.Logger, descriptions []*sarama.GroupDescription) ([]*GroupDescription, error) {
	response := make([]*GroupDescription, len(descriptions))
	for i, d := range descriptions {
		if d.Err != sarama.ErrNoError {
			return nil, d.Err
		}

		members, err := convertGroupMembers(logger, d.Members, d.ProtocolType)
		if err != nil {
			return nil, err
		}
		response[i] = &GroupDescription{
			GroupID:      d.GroupId,
			State:        d.State,
			ProtocolType: d.ProtocolType,
			Protocol:     d.Protocol,
			Members:      members,
		}
	}

	return response, nil
}

func convertGroupMembers(logger *zap.Logger, members map[string]*sarama.GroupMemberDescription, protocolType string) ([]*GroupMemberDescription, error) {
	response := make([]*GroupMemberDescription, len(members))

	counter := 0
	for id, m := range members {
		// MemberAssignments is a byte array which will be set by kafka clients. All clients which use protocol
		// type "consumer" are supposed to follow a schema which we will try to parse below. If the protocol type
		// is different we won't even try to deserialize the byte array as this will likely fail.
		//
		// Confluent's Schema registry for instance does not follow that schema and does therefore set a different
		// protocol type.
		// see: https://cwiki.apache.org/confluence/display/KAFKA/A+Guide+To+The+Kafka+Protocol

		resultAssignments := make([]*GroupMemberAssignment, 0)
		if protocolType == "consumer" {
			assignments, err := m.GetMemberAssignment()
			if err != nil {
				logger.Warn("failed to decode member assignments", zap.String("client_id", m.ClientId), zap.Error(err))
			}

			for topic, partitionIDs := range assignments.Topics {
				sort.Slice(partitionIDs, func(i, j int) bool { return partitionIDs[i] < partitionIDs[j] })

				a := &GroupMemberAssignment{
					TopicName:    topic,
					PartitionIDs: partitionIDs,
				}
				resultAssignments = append(resultAssignments, a)
			}
		}

		// Sort all assignments by topicname
		sort.Slice(resultAssignments, func(i, j int) bool {
			return resultAssignments[i].TopicName < resultAssignments[j].TopicName
		})

		response[counter] = &GroupMemberDescription{
			ID:          id,
			ClientID:    m.ClientId,
			ClientHost:  m.ClientHost,
			Assignments: resultAssignments,
		}
		counter++
	}

	return response, nil
}
