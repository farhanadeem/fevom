package daemon

type Waker interface {
	Woke()
}

func (n *Network) Woke() {
	if n.Mac != nil {
		n.Mac.Refresh("wake")
	}
}
