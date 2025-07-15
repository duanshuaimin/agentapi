package screentracker

// RingBuffer is a generic circular buffer that can store items of any type
type RingBuffer[T any] struct {
	items     []T
	nextIndex int
	count     int
	size      int
}

// NewRingBuffer creates a new ring buffer with the specified size
func NewRingBuffer[T any](size int) *RingBuffer[T] {
	return &RingBuffer[T]{
		items:     make([]T, size),
		nextIndex: 0,
		count:     0,
		size:      size,
	}
}

// Add adds an item to the ring buffer
func (r *RingBuffer[T]) Add(item T) {
	r.items[r.nextIndex] = item
	r.nextIndex = (r.nextIndex + 1) % len(r.items)
	if r.count < len(r.items) {
		r.count++
	}
}

// GetAll returns all items in the buffer, oldest first
func (r *RingBuffer[T]) GetAll() []T {
	result := make([]T, r.count)
	for i := 0; i < r.count; i++ {
		result[i] = r.items[(r.nextIndex-r.count+i+len(r.items))%len(r.items)]
	}
	return result
}

// Capacity returns the capacity of the ring buffer
func (r *RingBuffer[T]) Capacity() int {
	return r.size
}
