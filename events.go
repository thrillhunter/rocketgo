package rocket

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"
)

type ConnectEvent struct {
	Time time.Time
}

type DisconnectEvent struct {
	Time time.Time
	Err  error
}

type ErrorEvent struct {
	Time time.Time
	Raw  json.RawMessage
	Err  error
}

type MessageEvent struct {
	Action  string
	Time    time.Time
	Raw     json.RawMessage
	Message *Message
}

type RoomEvent struct {
	Action string
	Time   time.Time
	Raw    json.RawMessage
	Room   *Room
}

type SubscriptionEvent struct {
	Action       string
	Time         time.Time
	Raw          json.RawMessage
	Subscription *Subscription
}

type eventHandlerInstance struct {
	eventType    string
	eventHandler func(*Client, interface{})
}

var (
	clientHandlerType    = reflect.TypeOf((*Client)(nil))
	interfaceHandlerType = reflect.TypeOf((*interface{})(nil)).Elem()
)

func handlerForInterface(handler interface{}) (*eventHandlerInstance, error) {
	if handler == nil {
		return nil, errors.New("rocket: handler is nil")
	}

	value := reflect.ValueOf(handler)
	handlerType := value.Type()
	if handlerType.Kind() != reflect.Func {
		return nil, fmt.Errorf("rocket: handler must be a function, got %s", handlerType)
	}
	if handlerType.NumIn() != 2 {
		return nil, fmt.Errorf("rocket: handler must accept (*rocket.Client, event), got %d args", handlerType.NumIn())
	}
	if handlerType.NumOut() != 0 {
		return nil, errors.New("rocket: handler must not return values")
	}
	if handlerType.In(0) != clientHandlerType {
		return nil, fmt.Errorf("rocket: handler first argument must be *rocket.Client, got %s", handlerType.In(0))
	}

	eventType := handlerType.In(1)
	instance := &eventHandlerInstance{}
	if eventType == interfaceHandlerType {
		instance.eventHandler = func(client *Client, event interface{}) {
			value.Call([]reflect.Value{reflect.ValueOf(client), reflect.ValueOf(event)})
		}
		return instance, nil
	}
	if eventType.Kind() != reflect.Pointer || eventType.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("rocket: handler second argument must be interface{} or pointer to struct, got %s", eventType)
	}

	instance.eventType = eventType.String()
	instance.eventHandler = func(client *Client, event interface{}) {
		if reflect.TypeOf(event).String() != instance.eventType {
			return
		}
		value.Call([]reflect.Value{reflect.ValueOf(client), reflect.ValueOf(event)})
	}
	return instance, nil
}

// AddHandler registers a handler for a typed event.
//
// Example:
//
//	client.AddHandler(func(c *rocket.Client, evt *rocket.MessageEvent) {
//		fmt.Println(evt.Message.Text)
//	})
func (c *Client) AddHandler(handler interface{}) func() {
	instance, err := handlerForInterface(handler)
	if err != nil {
		if c.log != nil {
			c.log.Error("invalid event handler", "error", err)
		}
		return func() {}
	}
	return c.addEventHandler(instance, false)
}

// AddHandlerOnce registers a handler that runs once for the next matching event.
func (c *Client) AddHandlerOnce(handler interface{}) func() {
	instance, err := handlerForInterface(handler)
	if err != nil {
		if c.log != nil {
			c.log.Error("invalid once event handler", "error", err)
		}
		return func() {}
	}
	return c.addEventHandler(instance, true)
}

func (c *Client) addEventHandler(instance *eventHandlerInstance, once bool) func() {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()

	if once {
		if instance.eventType == "" {
			c.interfaceOnceHandlers = append(c.interfaceOnceHandlers, instance)
		} else {
			if c.onceHandlers == nil {
				c.onceHandlers = make(map[string][]*eventHandlerInstance)
			}
			c.onceHandlers[instance.eventType] = append(c.onceHandlers[instance.eventType], instance)
		}
	} else {
		if instance.eventType == "" {
			c.interfaceHandlers = append(c.interfaceHandlers, instance)
		} else {
			if c.handlers == nil {
				c.handlers = make(map[string][]*eventHandlerInstance)
			}
			c.handlers[instance.eventType] = append(c.handlers[instance.eventType], instance)
		}
	}

	return func() {
		c.removeEventHandlerInstance(instance)
	}
}

func (c *Client) removeEventHandlerInstance(instance *eventHandlerInstance) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()

	if instance.eventType == "" {
		c.interfaceHandlers = removeHandlerInstance(c.interfaceHandlers, instance)
		c.interfaceOnceHandlers = removeHandlerInstance(c.interfaceOnceHandlers, instance)
		return
	}

	if len(c.handlers[instance.eventType]) > 0 {
		c.handlers[instance.eventType] = removeHandlerInstance(c.handlers[instance.eventType], instance)
		if len(c.handlers[instance.eventType]) == 0 {
			delete(c.handlers, instance.eventType)
		}
	}
	if len(c.onceHandlers[instance.eventType]) > 0 {
		c.onceHandlers[instance.eventType] = removeHandlerInstance(c.onceHandlers[instance.eventType], instance)
		if len(c.onceHandlers[instance.eventType]) == 0 {
			delete(c.onceHandlers, instance.eventType)
		}
	}
}

func removeHandlerInstance(instances []*eventHandlerInstance, target *eventHandlerInstance) []*eventHandlerInstance {
	for i, instance := range instances {
		if instance == target {
			return append(instances[:i], instances[i+1:]...)
		}
	}
	return instances
}

func (c *Client) dispatchEvent(event interface{}) {
	if event == nil {
		return
	}

	select {
	case <-c.closed:
		return
	default:
	}

	eventType := reflect.TypeOf(event).String()
	interfaceHandlers, interfaceOnceHandlers, typedHandlers, typedOnceHandlers := c.snapshotHandlers(eventType)
	c.callHandlers(interfaceHandlers, event)
	c.callHandlers(interfaceOnceHandlers, event)
	c.callHandlers(typedHandlers, event)
	c.callHandlers(typedOnceHandlers, event)
}

func (c *Client) snapshotHandlers(eventType string) (
	interfaceHandlers []*eventHandlerInstance,
	interfaceOnceHandlers []*eventHandlerInstance,
	typedHandlers []*eventHandlerInstance,
	typedOnceHandlers []*eventHandlerInstance,
) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()

	interfaceHandlers = append([]*eventHandlerInstance(nil), c.interfaceHandlers...)
	interfaceOnceHandlers = append([]*eventHandlerInstance(nil), c.interfaceOnceHandlers...)
	c.interfaceOnceHandlers = nil

	if len(c.handlers[eventType]) > 0 {
		typedHandlers = append([]*eventHandlerInstance(nil), c.handlers[eventType]...)
	}
	if len(c.onceHandlers[eventType]) > 0 {
		typedOnceHandlers = append([]*eventHandlerInstance(nil), c.onceHandlers[eventType]...)
		c.onceHandlers[eventType] = nil
		delete(c.onceHandlers, eventType)
	}
	return
}

func (c *Client) callHandlers(instances []*eventHandlerInstance, event interface{}) {
	for _, instance := range instances {
		if c.SyncEvents {
			instance.eventHandler(c, event)
			continue
		}
		go instance.eventHandler(c, event)
	}
}
