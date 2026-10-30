import AppKit
import FevomDocker
import FevomStores
import SwiftUI

struct BuildsView: View {
    @Environment(AppModel.self) private var model
    @State private var selection: Set<BuildRecord.ID> = []
    @State private var filter = ""
    @State private var pendingDelete: BuildRecord?

    var rows: [BuildRecord] {
        let f = filter.trimmingCharacters(in: .whitespaces)
        guard !f.isEmpty else { return model.builds }
        return model.builds.filter { model.buildTitle($0).localizedCaseInsensitiveContains(f) || $0.name.localizedCaseInsensitiveContains(f) }
    }

    var detailBuild: BuildRecord? {
        guard selection.count == 1, let ref = selection.first else { return nil }
        return model.builds.first { $0.ref == ref }
    }

    var body: some View {
        let rows = rows
        ListDetail(idealDetail: 420, shown: detailBuild != nil, widthKey: "detailWidth.builds") {
            Table(rows, selection: $selection) {
                TableColumn("Name") { b in
                    HStack(spacing: 10) {
                        EntityIcon(symbol: "hammer", size: 26)
                        Text(model.buildTitle(b)).fontWeight(.medium).lineLimit(1).truncationMode(.middle)
                    }
                    .padding(.vertical, 5)
                }
                .width(min: 120, ideal: 320)
                TableColumn("Status") { b in BuildPill(build: b) }.width(min: 80, ideal: 110)
                TableColumn("Built") { b in
                    Text([b.duration.map(BuildFormat.duration), b.created.map(BuildFormat.ago)].compactMap { $0 }.joined(separator: " · "))
                        .monospacedDigit().foregroundStyle(.secondary).lineLimit(1)
                }
                .width(min: 70, ideal: 160)
            }
            .alternatingRowBackgrounds(.disabled)
            .contextMenu(forSelectionType: BuildRecord.ID.self) { ids in
                if let ref = ids.first, let b = model.builds.first(where: { $0.ref == ref }) {
                    Button("Copy Name") { copyToPasteboard(model.buildTitle(b)) }
                    Button("Copy Reference") { copyToPasteboard(b.ref) }
                    Divider()
                    Button("Delete Record…", role: .destructive) { pendingDelete = b }
                        .disabled(b.state == "running")
                }
            }
        } detail: {
            if let b = detailBuild {
                BuildDetailView(build: b, cached: model.cachedBuildSteps(b.ref)).id(b.ref)
            }
        }
        .searchable(text: $filter, prompt: "Filter builds")
        .findFocus()
        .toolbar {
            Button("Refresh", systemImage: "arrow.clockwise") { model.refreshList(.builds) }
                .help("Reload the build history")
        }
        .onDeleteCommand {
            if let ref = selection.first, let b = model.builds.first(where: { $0.ref == ref }), b.state != "running" { pendingDelete = b }
        }
        .confirmationDialog("Delete the record of \(pendingDelete.map(model.buildTitle) ?? "")?",
                            isPresented: Binding(get: { pendingDelete != nil }, set: { if !$0 { pendingDelete = nil } }), presenting: pendingDelete) { b in
            Button("Delete Record", role: .destructive) { Task { await model.removeBuild(b.ref) } }
            Button("Cancel", role: .cancel) {}
        } message: { b in
            Text("Its steps and logs are deleted; the image it built is kept.")
        }
        .overlay {
            if let e = model.buildsError, model.builds.isEmpty {
                WorkspaceEmptyState(symbol: "hammer", title: "The build history could not be read", message: e)
            } else if model.builds.isEmpty {
                WorkspaceEmptyState(symbol: "hammer", title: "No builds",
                                    command: "docker build -t app .")
            } else if rows.isEmpty {
                WorkspaceEmptyState(symbol: "magnifyingglass", title: "No matching builds")
            }
        }
        .workspacePage(summary: "\(model.builds.count) builds")
        .navigationTitle("Builds")
        .onAppear { model.watchList(.builds) }
        .onDisappear { model.unwatchList(.builds) }
        .onChange(of: model.builds.count) {
            if selection.isEmpty, DevHooks.value("FEV_APP_SELECT_FIRST") != nil, let f = model.builds.first { selection = [f.ref] }
        }
    }
}

struct BuildDetailView: View {
    @Environment(AppModel.self) private var model
    let build: BuildRecord
    @State private var steps: BuildSteps?

    init(build: BuildRecord, cached: BuildSteps?) {
        self.build = build
        _steps = State(initialValue: cached)
    }
    @State private var shown: StepFilter = .all
    @State private var internalSteps = false
    @State private var expanded: Set<String> = []

    enum StepFilter: String, CaseIterable, Identifiable {
        case all = "All steps", ran = "Ran", cached = "Cached", failed = "Failed"
        var id: String { rawValue }
    }

    var visible: [BuildStep] {
        (steps?.steps ?? []).filter { s in
            (internalSteps || !s.isInternal) && {
                switch shown {
                case .all: true
                case .ran: s.state == "completed" || s.state == "running"
                case .cached: s.state == "cached"
                case .failed: s.state == "failed"
                }
            }()
        }
    }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                HStack(alignment: .top, spacing: 12) {
                    EntityIcon(symbol: "hammer", size: 40)
                    VStack(alignment: .leading, spacing: 5) {
                        Text(model.buildTitle(build)).font(.headline).textSelection(.enabled).lineLimit(3)
                        BuildPill(build: build)
                    }
                }
                .padding(.vertical, 4)
                InspectorSection(title: "Overview") {
                    value("Duration", build.duration.map(BuildFormat.duration) ?? "Running")
                    if let d = build.created { value("Started", d.formatted(date: .abbreviated, time: .standard)) }
                    value("Steps", "\(build.completedSteps) of \(build.totalSteps)" + (build.cachedSteps > 0 ? ", \(build.cachedSteps) cached" : ""))
                    ForEach(model.buildTags[build.ref] ?? steps?.tags ?? [], id: \.self) { value("Tag", AppModel.familiar($0)) }
                    value("Reference", build.ref)
                }
                HStack(spacing: 10) {
                    Picker("Steps", selection: $shown) {
                        ForEach(StepFilter.allCases) { Text($0.rawValue).tag($0) }
                    }
                    .labelsHidden().fixedSize()
                    Toggle("Internal steps", isOn: $internalSteps).toggleStyle(.checkbox)
                        .help("Show BuildKit's own steps, such as loading the Dockerfile and the build context")
                    Spacer()
                    let all = Set(visible.filter { !$0.log.isEmpty || !$0.error.isEmpty }.map(\.digest))
                    Button(expanded.isSuperset(of: all) && !all.isEmpty ? "Collapse All" : "Expand All") {
                        expanded = expanded.isSuperset(of: all) ? [] : all
                    }
                    .disabled(all.isEmpty)
                }
                if steps == nil {
                    DelayedProgress().frame(maxWidth: .infinity).padding()
                } else if visible.isEmpty {
                    Text("No steps of this kind.").font(.callout).foregroundStyle(.secondary)
                } else {
                    VStack(spacing: 0) {
                        ForEach(visible) { s in
                            StepRow(step: s, total: build.duration ?? 0, expanded: Binding(
                                get: { expanded.contains(s.digest) },
                                set: { if $0 { expanded.insert(s.digest) } else { expanded.remove(s.digest) } }))
                            if s.digest != visible.last?.digest { Divider() }
                        }
                    }
                    .background(WorkspaceStyle.surface, in: RoundedRectangle(cornerRadius: 12))
                    .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(WorkspaceStyle.border, lineWidth: 0.5))
                }
            }
            .padding(18)
        }
        .background(WorkspaceStyle.canvas)
        .task(id: build.ref + build.status) {
            if steps != nil, build.state != "running", model.cachedBuildSteps(build.ref) != nil { return }
            steps = await model.buildSteps(build.ref) ?? BuildSteps(steps: [], tags: [])
        }
    }

    private func value(_ label: String, _ v: String) -> some View {
        HStack(alignment: .firstTextBaseline) {
            Text(label).foregroundStyle(.secondary)
            Spacer(minLength: 12)
            Text(v).multilineTextAlignment(.trailing).textSelection(.enabled).lineLimit(2).truncationMode(.middle)
        }
        .font(.callout)
    }
}

struct StepRow: View {
    let step: BuildStep
    let total: TimeInterval
    @Binding var expanded: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(alignment: .top, spacing: 8) {
                Image(systemName: expanded ? "chevron.down" : "chevron.right")
                    .font(.caption.weight(.semibold)).foregroundStyle(.secondary).frame(width: 10)
                    .opacity(hasMore ? 1 : 0)
                icon
                Text(step.name).font(.callout.monospaced()).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
                Spacer(minLength: 8)
                VStack(alignment: .trailing, spacing: 4) {
                    Text(step.state == "cached" ? "Cached" : step.duration.map(BuildFormat.duration) ?? "")
                        .font(.caption.monospacedDigit()).foregroundStyle(.secondary)
                    if total > 0, let d = step.duration, step.state != "cached" {
                        ProgressView(value: min(1, d / total)).progressViewStyle(.linear).frame(width: 70)
                    }
                }
            }
            .contentShape(Rectangle())
            .onTapGesture { if hasMore { expanded.toggle() } }
            if expanded {
                if !step.error.isEmpty {
                    Text(step.error).font(.caption.monospaced()).foregroundStyle(Color(nsColor: StatusAppearance.color("failed")))
                        .textSelection(.enabled)
                }
                if !step.log.isEmpty {
                    Text(step.log.trimmingCharacters(in: .newlines)).font(.caption.monospaced()).textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(8).background(WorkspaceStyle.canvas, in: RoundedRectangle(cornerRadius: 6))
                }
            }
        }
        .padding(.horizontal, 12).padding(.vertical, 9)
        .accessibilityElement(children: .combine)
    }

    var hasMore: Bool { !step.log.isEmpty || !step.error.isEmpty }

    @ViewBuilder var icon: some View {
        switch step.state {
        case "failed": Image(systemName: "xmark.circle.fill").foregroundStyle(Color(nsColor: StatusAppearance.color("failed")))
        case "cached": Image(systemName: "arrow.uturn.backward.circle.fill").foregroundStyle(.secondary)
        case "completed": Image(systemName: "checkmark.circle.fill").foregroundStyle(Color(nsColor: StatusAppearance.color("completed")))
        default: Image(systemName: "circle.dotted").foregroundStyle(.secondary)
        }
    }
}

struct BuildPill: View {
    let build: BuildRecord
    var body: some View {
        StatusPill(state: build.state, title: ["failed": "Failed", "canceled": "Canceled", "completed": "Completed"][build.state])
    }
}

enum BuildFormat {
    static func duration(_ t: TimeInterval) -> String {
        if t < 60 { return String(format: "%.1fs", t) }
        let m = Int(t) / 60, s = Int(t) % 60
        return m < 60 ? "\(m)m \(s)s" : "\(m / 60)h \(m % 60)m"
    }

    static func ago(_ d: Date) -> String {
        let f = RelativeDateTimeFormatter()
        f.unitsStyle = .abbreviated
        return f.localizedString(for: d, relativeTo: Date())
    }
}

struct DelayedProgress: View {
    @State private var shown = false
    var body: some View {
        ProgressView().opacity(shown ? 1 : 0)
            .task {
                try? await Task.sleep(for: .milliseconds(300))
                shown = true
            }
    }
}
