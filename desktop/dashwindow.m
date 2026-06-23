#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>

// A standalone dashboard window: a plain NSWindow hosting a WKWebView, separate
// from the Wails popover window. Because we own it (not Wails), its native
// traffic-light buttons work, it is freely movable/resizable, and closing it is
// just an orderOut — none of which disturbs the menu-bar popover.

static NSWindow *gDash = nil;
static WKWebView *gDashWeb = nil;
static id gDashDelegate = nil;
static id gDashKeyMonitor = nil;

@interface NSDashDelegate : NSObject <NSWindowDelegate>
@end
@implementation NSDashDelegate
// When the window closes, drop back to a menu-bar accessory app (no Dock icon)
// and tear down the Cmd-W monitor so it doesn't linger for the process lifetime.
- (void)windowWillClose:(NSNotification *)n {
  [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
  // Blank the web view so its live SSE stream stops and the WKWebView content
  // process is freed while the dashboard is closed (reopening reloads the URL).
  // NOTE: do NOT removeMonitor here — windowWillClose fires from inside the
  // Cmd-W monitor's own handler (via performClose:), and tearing the monitor
  // down mid-dispatch broke the close. The monitor is installed once for the
  // window's lifetime and is harmless when the dashboard is hidden (its handler
  // is gated on e.window == gDash).
  [gDashWeb loadHTMLString:@"" baseURL:nil];
}
@end

// NSExportHandler receives export requests from the dashboard JS and saves the
// file via a native Save panel. The web view can't write files itself, and
// WKWebView blob/attachment downloads are unreliable without a download
// delegate, so a JS→native message + NSSavePanel is the robust path.
static id gExportHandler = nil;
@interface NSExportHandler : NSObject <WKScriptMessageHandler>
@end
@implementation NSExportHandler
- (void)userContentController:(WKUserContentController *)ucc
      didReceiveScriptMessage:(WKScriptMessage *)msg {
  if (![msg.body isKindOfClass:[NSDictionary class]]) return;
  NSDictionary *body = (NSDictionary *)msg.body;
  NSString *text = body[@"text"];
  NSString *name = body[@"filename"];
  if (![text isKindOfClass:[NSString class]]) return;
  NSSavePanel *panel = [NSSavePanel savePanel];
  panel.nameFieldStringValue = [name isKindOfClass:[NSString class]] ? name : @"netscope-export.csv";
  void (^done)(NSModalResponse) = ^(NSModalResponse r) {
    if (r == NSModalResponseOK && panel.URL) {
      [[text dataUsingEncoding:NSUTF8StringEncoding] writeToURL:panel.URL atomically:YES];
    }
  };
  if (gDash) [panel beginSheetModalForWindow:gDash completionHandler:done];
  else done([panel runModal]);
}
@end

// installAppMenu gives the app a real main menu once. As a menu-bar accessory
// app we otherwise have none, so standard shortcuts (Cmd-W close, Cmd-Q quit,
// Cmd-C copy, Cmd-A select-all) don't work. Menu key equivalents are handled by
// AppKit before the focused WKWebView, so Cmd-W reliably closes the dashboard —
// unlike a local key monitor, which the web content can intercept.
static BOOL gMenuInstalled = NO;
static void installAppMenu(void) {
  if (gMenuInstalled) return;
  gMenuInstalled = YES;
  NSMenu *bar = [[NSMenu alloc] init];

  NSMenuItem *appItem = [[NSMenuItem alloc] init];
  [bar addItem:appItem];
  NSMenu *appMenu = [[NSMenu alloc] init];
  [appMenu addItemWithTitle:@"Quit netscope" action:@selector(terminate:) keyEquivalent:@"q"];
  [appItem setSubmenu:appMenu];

  NSMenuItem *editItem = [[NSMenuItem alloc] init];
  [bar addItem:editItem];
  NSMenu *editMenu = [[NSMenu alloc] initWithTitle:@"Edit"];
  [editMenu addItemWithTitle:@"Copy" action:@selector(copy:) keyEquivalent:@"c"];
  [editMenu addItemWithTitle:@"Select All" action:@selector(selectAll:) keyEquivalent:@"a"];
  [editItem setSubmenu:editMenu];

  NSMenuItem *winItem = [[NSMenuItem alloc] init];
  [bar addItem:winItem];
  NSMenu *winMenu = [[NSMenu alloc] initWithTitle:@"Window"];
  [winMenu addItemWithTitle:@"Minimize" action:@selector(performMiniaturize:) keyEquivalent:@"m"];
  [winMenu addItemWithTitle:@"Close" action:@selector(performClose:) keyEquivalent:@"w"];
  [winItem setSubmenu:winMenu];

  [NSApp setMainMenu:bar];
}

// openDashWindow creates (or re-focuses) the dashboard window and loads url.
void openDashWindow(const char *curl) {
  NSString *urlStr = [NSString stringWithUTF8String:curl];
  dispatch_async(dispatch_get_main_queue(), ^{
    installAppMenu();
    // Become a regular app so the window gains focus, Cmd-Tab and a Dock icon.
    [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
    // A runtime-promoted accessory app shows a generic (white) icon in Cmd-Tab /
    // the Dock — and it's already non-nil, so set our bundle icon unconditionally
    // (once). Load the bundle's actual composited icon via NSWorkspace (more
    // reliable than reading the .icns directly), then force the Dock tile to
    // redraw so the freshly-promoted process picks it up instead of the generic.
    static BOOL gIconSet = NO;
    if (!gIconSet) {
      NSImage *img = [[NSWorkspace sharedWorkspace] iconForFile:[[NSBundle mainBundle] bundlePath]];
      if (!img) {
        NSString *icon = [[NSBundle mainBundle] pathForResource:@"iconfile" ofType:@"icns"];
        img = icon ? [[NSImage alloc] initWithContentsOfFile:icon] : nil;
      }
      if (img) {
        NSApp.applicationIconImage = img;
        [NSApp.dockTile display];
        gIconSet = YES;
      }
    }
    if (gDash == nil) {
      NSRect frame = NSMakeRect(0, 0, 1120, 760);
      // No FullSizeContentView: that lets the WKWebView fill the title-bar strip
      // and swallow its mouse events, so the window can't be dragged. A normal
      // (transparent) title bar gives a real draggable strip with traffic lights.
      NSUInteger mask = NSWindowStyleMaskTitled | NSWindowStyleMaskClosable |
                        NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable;
      gDash = [[NSWindow alloc] initWithContentRect:frame
                                          styleMask:mask
                                            backing:NSBackingStoreBuffered
                                              defer:NO];
      gDash.releasedWhenClosed = NO;                 // reuse across open/close
      gDash.title = @"netscope";
      gDash.titlebarAppearsTransparent = YES;        // blend the bar into the UI
      gDash.titleVisibility = NSWindowTitleHidden;
      gDash.appearance = [NSAppearance appearanceNamed:NSAppearanceNameDarkAqua];
      gDash.backgroundColor = [NSColor colorWithSRGBRed:13/255.0 green:17/255.0 blue:23/255.0 alpha:1.0];
      [gDash setMinSize:NSMakeSize(760, 480)];
      gDashDelegate = [[NSDashDelegate alloc] init];
      gDash.delegate = gDashDelegate;

      // Cmd-W to close. As a menu-bar accessory app we have no application menu,
      // so the standard File ▸ Close item that binds Cmd-W doesn't exist, and a
      // window performKeyEquivalent: override is intercepted by the focused
      // WKWebView (its content process eats the key event). A local event monitor
      // sees the key down before the responder chain, so it reliably closes the
      // window even while the web view has focus. Installed once for the window's
      // lifetime (the e.window == gDash gate keeps it inert when the dashboard
      // isn't the target); never removed — doing so from windowWillClose: ran
      // inside this handler and broke the close.
      gDashKeyMonitor = [[NSEvent addLocalMonitorForEventsMatchingMask:NSEventMaskKeyDown
                         handler:^NSEvent *(NSEvent *e) {
        if ((e.modifierFlags & NSEventModifierFlagCommand) &&
            [[e charactersIgnoringModifiers] isEqualToString:@"w"] &&
            e.window == gDash) {
          [gDash performClose:nil];
          return nil; // consume the event
        }
        return e;
      }] retain];

      WKWebViewConfiguration *cfg = [[WKWebViewConfiguration alloc] init];
      // Expose window.webkit.messageHandlers.netscopeExport for CSV/JSON export.
      gExportHandler = [[NSExportHandler alloc] init];
      [cfg.userContentController addScriptMessageHandler:gExportHandler name:@"netscopeExport"];
      gDashWeb = [[WKWebView alloc] initWithFrame:frame configuration:cfg];
      gDashWeb.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
      gDash.contentView = gDashWeb;
      [gDash center];
    }
    NSURL *url = [NSURL URLWithString:urlStr];
    [gDashWeb loadRequest:[NSURLRequest requestWithURL:url]];
    [gDash makeKeyAndOrderFront:nil];
    [NSApp activateIgnoringOtherApps:YES];
  });
}

// closeDashWindow hides the dashboard window (kept for reuse).
void closeDashWindow(void) {
  dispatch_async(dispatch_get_main_queue(), ^{
    if (gDash) [gDash orderOut:nil];
  });
}
