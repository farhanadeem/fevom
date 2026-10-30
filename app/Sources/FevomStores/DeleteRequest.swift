import FevomAPI
import FevomDocker
import Foundation

public enum DeleteRequest: Identifiable, Equatable, Sendable {
    case container(Container)
    case project(String, [Container])
    case machine(String)

    public static func project(named p: String, in containers: [Container]) -> DeleteRequest {
        .project(p, containers.filter { $0.composeProject == p })
    }

    public var id: String {
        switch self {
        case .container(let c): "container:" + c.id
        case .project(let p, _): "project:" + p
        case .machine(let m): "machine:" + m
        }
    }

    public var title: String {
        switch self {
        case .container(let c): "Delete the container \(c.name)?"
        case .project(let p, let cs): "Delete the Compose project \(p) and its \(cs.count == 1 ? "container" : "\(cs.count) containers")?"
        case .machine(let m): "Delete \(m) and all of its files?"
        }
    }

    public var message: String {
        switch self {
        case .container(let c):
            let running = c.state == "running" || c.state == "paused"
            return (running ? "It's stopped first. " : "") + "Its files are deleted; its volumes are kept."
        case .project(_, let cs):
            let names = cs.map(\.name).sorted().joined(separator: ", ")
            let running = cs.filter { $0.state == "running" || $0.state == "paused" }.count
            return "\(names). " + (running > 0 ? "Running containers are stopped first. " : "")
                + "The project's networks, volumes and Compose file are kept."
        case .machine:
            return "Everything in it is deleted. This can't be undone."
        }
    }

    public var button: String {
        switch self {
        case .container: "Delete Container"
        case .project: "Delete Project"
        case .machine: "Delete Machine"
        }
    }
}

extension AppModel {
    public func perform(_ r: DeleteRequest) async {
        switch r {
        case .container(let c): await removeContainer(c.id, force: true)
        case .project(let p, _): await project(p, "delete")
        case .machine(let m): await deleteMachine(m)
        }
    }
}
