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
  if (gDashKeyMonitor) {
    [NSEvent removeMonitor:gDashKeyMonitor];
    gDashKeyMonitor = nil;
  }
}
@end

// openDashWindow creates (or re-focuses) the dashboard window and loads url.
void openDashWindow(const char *curl) {
  NSString *urlStr = [NSString stringWithUTF8String:curl];
  dispatch_async(dispatch_get_main_queue(), ^{
    // Become a regular app so the window gains focus, Cmd-Tab and a Dock icon.
    [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
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

      WKWebViewConfiguration *cfg = [[WKWebViewConfiguration alloc] init];
      gDashWeb = [[WKWebView alloc] initWithFrame:frame configuration:cfg];
      gDashWeb.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
      gDash.contentView = gDashWeb;
      [gDash center];
    }
    NSURL *url = [NSURL URLWithString:urlStr];
    [gDashWeb loadRequest:[NSURLRequest requestWithURL:url]];
    [gDash makeKeyAndOrderFront:nil];
    [NSApp activateIgnoringOtherApps:YES];

    // Cmd-W to close. As a menu-bar accessory app we have no application menu,
    // so the standard File ▸ Close item that binds Cmd-W doesn't exist, and a
    // window performKeyEquivalent: override is intercepted by the focused
    // WKWebView (its content process eats the key event). A local event monitor
    // sees the key down before the responder chain, so it reliably closes the
    // window even while the web view has focus. Installed per-open and removed
    // in windowWillClose: so it never lingers eating Cmd-W app-wide.
    if (!gDashKeyMonitor) {
      gDashKeyMonitor = [NSEvent addLocalMonitorForEventsMatchingMask:NSEventMaskKeyDown
                         handler:^NSEvent *(NSEvent *e) {
        if ((e.modifierFlags & NSEventModifierFlagCommand) &&
            [[e charactersIgnoringModifiers] isEqualToString:@"w"] &&
            e.window == gDash) {
          [gDash performClose:nil];
          return nil; // consume the event
        }
        return e;
      }];
    }
  });
}

// closeDashWindow hides the dashboard window (kept for reuse).
void closeDashWindow(void) {
  dispatch_async(dispatch_get_main_queue(), ^{
    if (gDash) [gDash orderOut:nil];
  });
}
