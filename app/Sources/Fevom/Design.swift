import AppKit
import SwiftUI

enum WorkspaceStyle {
    static let canvas = Color(nsColor: .windowBackgroundColor)
    static let surface = Color(nsColor: .controlBackgroundColor)
    static let border = Color(nsColor: .separatorColor).opacity(0.35)
    static let inset: CGFloat = 24
}

struct EntityIcon: View {
    let symbol: String
    var color: Color = .accentColor
    var size: CGFloat = 32

    var body: some View {
        Image(systemName: symbol)
            .font(.system(size: size * 0.44, weight: .medium))
            .foregroundStyle(color)
            .frame(width: size, height: size)
            .background(color.opacity(0.10), in: RoundedRectangle(cornerRadius: size * 0.25))
            .accessibilityHidden(true)
    }
}

struct ListSummary: View {
    let text: String

    var body: some View {
        HStack {
            Text(text)
            Spacer()
        }
        .font(.caption)
        .foregroundStyle(.secondary)
        .padding(.horizontal, WorkspaceStyle.inset)
        .padding(.vertical, 11)
        .background(WorkspaceStyle.canvas)
    }
}

enum StatusAppearance {
    static func title(_ state: String) -> String {
        switch state {
        case "running", "ready", "online": "Running"
        case "exited", "stopped", "created", "dead": "Stopped"
        case "paused": "Paused"
        case "starting": "Starting"
        case "stopping": "Stopping"
        case "failed", "error": "Needs attention"
        default: state.prefix(1).uppercased() + state.dropFirst()
        }
    }

    private static let success = adaptive(light: NSColor(srgbRed: 0.12, green: 0.45, blue: 0.22, alpha: 1), dark: .systemGreen)
    private static let pending = adaptive(light: NSColor(srgbRed: 0.60, green: 0.34, blue: 0, alpha: 1), dark: .systemOrange)
    private static let failure = adaptive(light: NSColor(srgbRed: 0.70, green: 0.14, blue: 0.10, alpha: 1), dark: .systemRed)

    private static func adaptive(light: NSColor, dark: NSColor) -> NSColor {
        NSColor(name: nil) { appearance in
            appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua ? dark : light
        }
    }

    static func color(_ state: String) -> NSColor {
        switch state {
        case "running", "ready", "online", "completed": success
        case "starting", "stopping", "paused": pending
        case "failed", "error": failure
        default: .secondaryLabelColor
        }
    }
}

struct StatusPill: View {
    let state: String
    var title: String?

    var body: some View {
        let color = Color(nsColor: StatusAppearance.color(state))
        HStack(spacing: 5) {
            Circle().fill(color).frame(width: 5, height: 5)
            Text(title ?? StatusAppearance.title(state)).font(.system(size: 11, weight: .medium)).lineLimit(1)
        }
        .foregroundStyle(color)
        .padding(.horizontal, 8)
        .padding(.vertical, 4)
        .background(color.opacity(0.09), in: Capsule())
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(title ?? StatusAppearance.title(state))
    }
}

struct WorkspaceEmptyState: View {
    let symbol: String
    let title: String
    var message: String = ""
    var command: String?
    var actionTitle: String?
    var action: (() -> Void)?

    var body: some View {
        VStack(spacing: 14) {
            EntityIcon(symbol: symbol, size: 58).padding(.bottom, 4)
            Text(title).font(.title2.weight(.semibold))
            if !message.isEmpty {
                Text(message).font(.callout).foregroundStyle(.secondary)
                    .multilineTextAlignment(.center).fixedSize(horizontal: false, vertical: true)
                    .frame(maxWidth: 350)
            }
            if let command {
                CommandLine(text: command)
                    .padding(12)
                    .background(WorkspaceStyle.canvas, in: RoundedRectangle(cornerRadius: 9))
                    .frame(maxWidth: 370)
                    .padding(.top, 4)
            }
            if let actionTitle, let action {
                Button(actionTitle, action: action).buttonStyle(.borderedProminent).controlSize(.large)
                    .padding(.top, 4)
            }
        }
        .padding(32)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(WorkspaceStyle.surface)
    }
}

struct InspectorSection<Content: View>: View {
    let title: String
    @ViewBuilder let content: Content

    var body: some View {
        VStack(alignment: .leading, spacing: 13) {
            Text(title).font(.system(size: 11, weight: .semibold)).foregroundStyle(.secondary)
                .textCase(.uppercase).tracking(0.6)
            content
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(18)
        .background(WorkspaceStyle.surface, in: RoundedRectangle(cornerRadius: 12))
        .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(WorkspaceStyle.border, lineWidth: 0.5))
    }
}

struct ListDetail<List: View, Detail: View>: View {
    let idealDetail: CGFloat
    let shown: Bool
    @ViewBuilder let list: List
    @ViewBuilder let detail: Detail
    @AppStorage private var width: Double
    @State private var dragFrom: CGFloat?

    init(idealDetail: CGFloat, shown: Bool, widthKey: String, @ViewBuilder list: () -> List, @ViewBuilder detail: () -> Detail) {
        self.idealDetail = idealDetail
        self.shown = shown
        self.list = list()
        self.detail = detail()
        _width = AppStorage(wrappedValue: Double(idealDetail), widthKey)
    }

    static func fit(_ w: CGFloat, in total: CGFloat) -> CGFloat {
        max(280, min(w, 640, total - 300 - 1))
    }

    var body: some View {
        GeometryReader { g in
            let w = Self.fit(width, in: g.size.width)
            HStack(spacing: 0) {
                list.frame(minWidth: 0, maxWidth: .infinity, maxHeight: .infinity)
                if shown {
                    Rectangle().fill(Color(nsColor: .separatorColor)).frame(width: 1)
                        .overlay {
                            Color.clear.frame(width: 9).contentShape(Rectangle())
                                .pointerStyle(.columnResize)
                                .gesture(DragGesture(minimumDistance: 1, coordinateSpace: .global)
                                    .onChanged { v in
                                        let from = dragFrom ?? w
                                        dragFrom = from
                                        width = Double(Self.fit(from - v.translation.width, in: g.size.width))
                                    }
                                    .onEnded { _ in dragFrom = nil })
                                .onTapGesture(count: 2) { width = Double(idealDetail) }
                                .accessibilityLabel("Resize the detail")
                        }
                    detail.frame(width: w).frame(maxHeight: .infinity)
                }
            }
        }
    }
}

extension View {
    func workspacePage(summary: String) -> some View {
        VStack(spacing: 0) {
            GeometryReader { geometry in
                self.frame(width: geometry.size.width, height: geometry.size.height)
            }
            .clipped()
            Divider().overlay(WorkspaceStyle.border)
            ListSummary(text: summary)
        }
        .background(WorkspaceStyle.canvas)
    }
}
