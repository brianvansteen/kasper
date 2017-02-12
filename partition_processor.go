package kasper

import (
	"log"

	"github.com/Shopify/sarama"
)

type partitionProcessor struct {
	topicProcessor                 *TopicProcessor
	coordinator                    Coordinator
	consumer                       sarama.Consumer
	partitionConsumers             []sarama.PartitionConsumer
	offsetManagers                 map[string]sarama.PartitionOffsetManager
	messageProcessor               MessageProcessor
	inputTopics                    []string
	partition                      int
	inFlightMessageGroups          map[string][]*inFlightMessageGroup
	commitNextInFlightMessageGroup bool
}

func (pp *partitionProcessor) consumerMessageChannels() []<-chan *sarama.ConsumerMessage {
	chans := make([]<-chan *sarama.ConsumerMessage, len(pp.partitionConsumers))
	for i, consumer := range pp.partitionConsumers {
		chans[i] = consumer.Messages()
	}
	return chans
}

func newPartitionProcessor(tp *TopicProcessor, mp MessageProcessor, partition int) *partitionProcessor {
	consumer, err := sarama.NewConsumerFromClient(tp.client)
	if err != nil {
		log.Fatal(err)
	}
	partitionConsumers := make([]sarama.PartitionConsumer, len(tp.inputTopics))
	partitionOffsetManagers := make(map[string]sarama.PartitionOffsetManager)
	for i, topic := range tp.inputTopics {
		pom, err := tp.offsetManager.ManagePartition(string(topic), int32(partition))
		if err != nil {
			log.Fatal(err)
		}
		newestOffset, err := tp.client.GetOffset(string(topic), int32(partition), sarama.OffsetNewest)
		if err != nil {
			log.Fatal(err)
		}
		nextOffset, _ := pom.NextOffset()
		if nextOffset > newestOffset {
			nextOffset = sarama.OffsetNewest
		}
		c, err := consumer.ConsumePartition(string(topic), int32(partition), nextOffset)
		if err != nil {
			log.Fatal(err)
		}
		partitionConsumers[i] = c
		partitionOffsetManagers[topic] = pom
	}
	pp := &partitionProcessor{
		tp,
		nil,
		consumer,
		partitionConsumers,
		partitionOffsetManagers,
		mp,
		tp.inputTopics,
		partition,
		make(map[string][]*inFlightMessageGroup),
		false,
	}
	pp.coordinator = &partitionProcessorCoordinator{pp}
	return pp
}

func (pp *partitionProcessor) process(consumerMessage *sarama.ConsumerMessage) []*sarama.ProducerMessage {
	topicSerde, ok := pp.topicProcessor.config.TopicSerdes[string(consumerMessage.Topic)]
	if !ok {
		log.Fatalf("Could not find Serde for topic '%s'", consumerMessage.Topic)
	}
	incomingMessage := IncomingMessage{
		Topic:     consumerMessage.Topic,
		Partition: int(consumerMessage.Partition),
		Offset:    consumerMessage.Offset,
		Key:       topicSerde.KeySerde.Deserialize(consumerMessage.Key),
		Value:     topicSerde.ValueSerde.Deserialize(consumerMessage.Value),
		Timestamp: consumerMessage.Timestamp,
	}
	sender := newSender(pp, &incomingMessage)
	pp.commitNextInFlightMessageGroup = false
	pp.messageProcessor.Process(incomingMessage, sender, pp.coordinator)
	inFlightMessageGroup := sender.createInFlightMessageGroup(pp.commitNextInFlightMessageGroup)
	pp.inFlightMessageGroups[consumerMessage.Topic] = append(
		pp.inFlightMessageGroups[consumerMessage.Topic],
		inFlightMessageGroup,
	)
	return sender.producerMessages
}

func (pp *partitionProcessor) onProcessCompleted() {
	pp.pruneInFlightMessageGroups()
}

func (pp *partitionProcessor) pruneInFlightMessageGroups() {
	for _, topic := range pp.topicProcessor.inputTopics {
		pp.pruneInFlightMessageGroupsForTopic(topic)
	}
}

func (pp *partitionProcessor) pruneInFlightMessageGroupsForTopic(topic string) {
	for len(pp.inFlightMessageGroups[topic]) > 1 {
		headGroup := pp.inFlightMessageGroups[topic][0]
		nextGroup := pp.inFlightMessageGroups[topic][1]
		if !headGroup.allAcksAreTrue() || !nextGroup.allAcksAreTrue() {
			break
		}
		pp.inFlightMessageGroups[topic] = pp.inFlightMessageGroups[topic][1:]
	}
}

func (pp *partitionProcessor) isReadyForMessage(msg *sarama.ConsumerMessage) bool {
	maxGroups := pp.topicProcessor.config.Config.MaxInFlightMessageGroups
	return len(pp.inFlightMessageGroups[msg.Topic]) <= maxGroups
}

func (pp *partitionProcessor) markOffsetsIfPossible() {
	for _, topic := range pp.topicProcessor.inputTopics {
		pp.markOffsetsForTopicIfPossible(topic)
	}
}

func (pp *partitionProcessor) markOffsetsForTopicIfPossible(topic string) {
	var offset int64 = -1
	for len(pp.inFlightMessageGroups[topic]) > 0 {
		group := pp.inFlightMessageGroups[topic][0]
		if !group.allAcksAreTrue() {
			break
		}
		offset = group.incomingMessage.Offset
		if group.committed && pp.topicProcessor.config.markOffsetsManually() {
			offsetManager := pp.offsetManagers[topic]
			offsetManager.MarkOffset(offset+1, "")
		}
		pp.inFlightMessageGroups[topic] = pp.inFlightMessageGroups[topic][1:]
	}
	if offset != -1 && pp.topicProcessor.config.markOffsetsAutomatically() {
		offsetManager := pp.offsetManagers[topic]
		offsetManager.MarkOffset(offset+1, "")
	}
}

func (pp *partitionProcessor) onProducerAck(sentMessage *sarama.ProducerMessage) {
	incomingMessage := sentMessage.Metadata.(*IncomingMessage)
	foundGroup := false
	for _, group := range pp.inFlightMessageGroups[incomingMessage.Topic] {
		if group.incomingMessage == incomingMessage {
			foundGroup = true
			foundMsg := false
			for _, inFlightMessage := range group.inFlightMessages {
				if inFlightMessage.msg == sentMessage {
					foundMsg = true
					inFlightMessage.ack = true
					break
				}
			}
			if !foundMsg {
				log.Fatal("Could not find producer message in inFlightMessageGroups")
			}
			break
		}
	}
	if !foundGroup {
		log.Fatal("Could not find group in inFlightMessageGroups")
	}
}

func (pp *partitionProcessor) onShutdown() {
	for _, pom := range pp.offsetManagers {
		pom.Close()
	}
	for _, pc := range pp.partitionConsumers {
		pc.Close()
	}
	pp.consumer.Close()
}
