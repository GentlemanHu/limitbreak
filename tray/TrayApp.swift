import AppKit
import Combine
import SwiftUI

@main
struct LimitBreakApp {
    @MainActor static func main() {
        let app = NSApplication.shared
        let delegate = TrayDelegate()
        DockPresence.apply(windowOpen: false)
        app.delegate = delegate
        withExtendedLifetime(delegate) { app.run() }
    }
}

@MainActor
final class TrayDelegate: NSObject, NSApplicationDelegate {
    let runtime = TrayRuntime()
    private var item: NSStatusItem!
    private let popover = NSPopover()
    private var subscription: AnyCancellable?
    private var activation: NSObjectProtocol?
    private var workbench: WorkbenchWindow?
    private var recovery: RecoveryWindow?
    private var shownSymbol = "bolt.horizontal.circle"

    func applicationDidFinishLaunching(_ notification: Notification) {
        installMenu()
        item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        item.button?.image = NSImage(systemSymbolName: "bolt.horizontal.circle", accessibilityDescription: "LimitBreak")
        item.button?.imagePosition = .imageLeading
        item.button?.target = self
        item.button?.action = #selector(togglePopover)
        item.button?.toolTip = "点击查看账号额度和 Claude 会话"
        popover.behavior = .transient
        popover.animates = false
        let panel = NSHostingController(rootView: TrayPanel(runtime: runtime))
        // The panel grows and shrinks with the number of accounts and waiting sessions.
        panel.sizingOptions = .preferredContentSize
        popover.contentViewController = panel
        // Coming back from System Settings re-reads the accessibility switch immediately.
        activation = NotificationCenter.default.addObserver(forName: NSApplication.didBecomeActiveNotification, object: nil, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.runtime.checkAccessibility(); self?.runtime.detectHooks() }
        }
        runtime.showWorkbench = { [weak self] section in self?.openWorkbench(section) }
        runtime.showRecoveryWindow = { [weak self] in self?.openRecovery() }
        subscription = runtime.objectWillChange.receive(on: RunLoop.main).sink { [weak self] _ in
            guard let self else { return }
            self.item.button?.title = " " + self.runtime.title
            self.updateSymbol()
        }
        Task { await runtime.start() }
        item.button?.title = " " + runtime.title
        // Explicit application opens have a real native window, not a transient web or menu panel.
        if !CommandLine.arguments.contains("--background") {
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.6) { [weak self] in self?.openWorkbench(.overview) }
        }
    }
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        openWorkbench(.overview)
        return true
    }
    private func installMenu() {
        let menu = NSMenu()
        let appItem = NSMenuItem()
        let appMenu = NSMenu(title: "LimitBreak")
        appMenu.addItem(withTitle: "退出 LimitBreak", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu; menu.addItem(appItem)
        let editItem = NSMenuItem()
        let editMenu = NSMenu(title: "编辑")
        editMenu.addItem(withTitle: "撤销", action: Selector(("undo:")), keyEquivalent: "z")
        editMenu.addItem(.separator())
        editMenu.addItem(withTitle: "剪切", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        editMenu.addItem(withTitle: "复制", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        editMenu.addItem(withTitle: "粘贴", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        editMenu.addItem(withTitle: "全选", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = editMenu; menu.addItem(editItem)
        let windowItem = NSMenuItem()
        let windowMenu = NSMenu(title: "窗口")
        windowMenu.addItem(withTitle: "关闭工作台", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        windowMenu.addItem(withTitle: "最小化", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        windowItem.submenu = windowMenu; menu.addItem(windowItem)
        NSApplication.shared.mainMenu = menu
        NSApplication.shared.windowsMenu = windowMenu
    }
    func applicationWillTerminate(_ notification: Notification) { runtime.shutdown() }
    @objc private func togglePopover() {
        if popover.isShown { popover.performClose(nil) } else { showPopover() }
    }
    private func showPopover() {
        guard let button = item?.button else { return }
        popover.show(relativeTo: button.bounds, of: button, preferredEdge: .minY)
        popover.contentViewController?.view.window?.makeKey()
    }
    private func updateSymbol() {
        let symbol = runtime.statusSymbol
        guard symbol != shownSymbol else { return }
        shownSymbol = symbol
        let image = NSImage(systemSymbolName: symbol, accessibilityDescription: "LimitBreak")
        image?.isTemplate = true
        item.button?.image = image
    }
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }
    private func openWorkbench(_ section: WorkbenchSection) {
        popover.performClose(nil)
        if workbench == nil { workbench = WorkbenchWindow(runtime: runtime, closed: { [weak self] in Task { @MainActor in self?.syncDock() } }) }
        DockPresence.apply(windowOpen: true)
        workbench?.show(section)
    }
    private func openRecovery() {
        popover.performClose(nil)
        if recovery == nil { recovery = RecoveryWindow(runtime: runtime, closed: { [weak self] in Task { @MainActor in self?.syncDock() } }) }
        DockPresence.apply(windowOpen: true)
        recovery?.show()
    }
    // The Dock icon stays while either window is open.
    private func syncDock() {
        DockPresence.apply(windowOpen: NSApplication.shared.windows.contains { $0.isVisible && $0.styleMask.contains(.titled) })
    }
}

// A menu-bar app: the Dock icon appears while the workbench is open, or always if the user asks.
enum DockPresence {
    static let key = "QSOShowInDock"
    @MainActor static func apply(windowOpen: Bool) {
        let always = UserDefaults.standard.bool(forKey: key)
        let policy: NSApplication.ActivationPolicy = always || windowOpen ? .regular : .accessory
        if NSApplication.shared.activationPolicy() != policy { NSApplication.shared.setActivationPolicy(policy) }
    }
}
