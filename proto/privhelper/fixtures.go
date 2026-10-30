package privhelper

import (
	"embed"

	"fev/proto/contract"
)

//go:embed fixtures
var fixtureFiles embed.FS

type Fixture = contract.Fixture

func Fixtures() ([]Fixture, error) { return contract.Load(fixtureFiles, "fixtures") }

func PendingFixtures() ([]Fixture, error) { return contract.Load(fixtureFiles, "fixtures/pending") }

var Schema = map[string]contract.Types{
	MethodHello:            {Empty{}, HelloResult{}},
	MethodTunnelOpen:       {TunnelParams{}, Tunnel{}},
	MethodRoutesSet:        {RoutesParams{}, Routes{}},
	MethodPortBind:         {PortParams{}, Bound{}},
	MethodDockerSockLink:   {Empty{}, DockerSock{}},
	MethodDockerSockUnlink: {Empty{}, DockerSock{}},
	MethodLocalNamesSet:    {LocalNamesParams{}, LocalNames{}},
}

var Pending = map[string]contract.Types{
	MethodResolverSet:   {Empty{}, Resolver{}},
	MethodResolverClear: {Empty{}, Resolver{}},
}

func Conforms(data []byte, zero any) error { return contract.Conforms(data, zero) }
