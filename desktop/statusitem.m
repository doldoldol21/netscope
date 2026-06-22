#import <Cocoa/Cocoa.h>

// Implemented in Go (//export).
void statusItemClickedGo(void);
void popoverDidHideGo(void);

@interface NSStatusTarget : NSObject
- (void)clicked:(id)sender;
@end
@implementation NSStatusTarget
- (void)clicked:(id)sender { statusItemClickedGo(); }
@end

static NSStatusItem *gItem = nil;
static NSStatusTarget *gTarget = nil;

// installStatusItem creates the menu-bar status item with a template image.
void installStatusItem(const void *png, int len) {
  dispatch_async(dispatch_get_main_queue(), ^{
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    gItem = [[[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength] retain];
    NSData *d = [NSData dataWithBytes:png length:len];
    NSImage *img = [[NSImage alloc] initWithData:d];
    [img setTemplate:YES];
    [img setSize:NSMakeSize(18, 18)];
    gItem.button.image = img;
    gTarget = [[NSStatusTarget alloc] init];
    gItem.button.target = gTarget;
    gItem.button.action = @selector(clicked:);
  });
}

// positionPopover places the Wails popover flush under the status item, on
// whichever display the menu bar (and the item) currently lives.
//
// We set the NSWindow frame directly in global screen coordinates rather than
// going through Wails' WindowSetPosition. Wails positions relative to the
// window's *current* screen (and we previously anchored off [NSScreen
// mainScreen]), so on multi-display setups the popover landed on the wrong
// monitor or at the wrong offset. The status item's own window frame is already
// in the global coordinate space spanning all displays, so anchoring to it is
// correct everywhere.
void positionPopover(int winW, int winH) {
  dispatch_sync(dispatch_get_main_queue(), ^{
    if (!gItem) return;
    NSWindow *btnWin = gItem.button.window;
    NSScreen *screen = btnWin.screen ?: [NSScreen mainScreen];
    NSRect f = btnWin.frame;                       // global coords, bottom-left origin
    CGFloat cx = f.origin.x + f.size.width / 2.0;  // status item centre (global x)
    NSRect vis = screen.visibleFrame;

    CGFloat left = cx - winW / 2.0;
    // Keep the panel fully on its display.
    CGFloat minX = vis.origin.x + 4;
    CGFloat maxX = vis.origin.x + vis.size.width - winW - 4;
    if (left < minX) left = minX;
    if (left > maxX) left = maxX;
    CGFloat top = f.origin.y - 2;                  // a hair below the status item
    NSRect frame = NSMakeRect(left, top - winH, winW, winH);

    for (NSWindow *w in [NSApp windows]) {
      if ([w isKindOfClass:NSClassFromString(@"WailsWindow")]) {
        [w setFrame:frame display:YES];
        break;
      }
    }
  });
}

// enablePopoverDismiss hides the popover window when it loses key focus (the
// user clicks elsewhere) — the native menu-bar popover behaviour.
static id gResignObs = nil;
void enablePopoverDismiss(void) {
  dispatch_async(dispatch_get_main_queue(), ^{
    if (gResignObs) return;
    gResignObs = [[NSNotificationCenter defaultCenter]
      addObserverForName:NSWindowDidResignKeyNotification
      object:nil
      queue:[NSOperationQueue mainQueue]
      usingBlock:^(NSNotification *n) {
        NSWindow *w = (NSWindow *)n.object;
        // Only the Wails popover dismisses on blur; the dashboard is its own
        // (plain NSWindow) and must stay put when the user clicks elsewhere.
        if (![w isKindOfClass:NSClassFromString(@"WailsWindow")]) return;
        if (w && w.isVisible) { [w orderOut:nil]; popoverDidHideGo(); }
      }];
  });
}

// focusPopover makes the popover window key so it can later resign (dismiss).
void focusPopover(void) {
  dispatch_async(dispatch_get_main_queue(), ^{
    NSWindow *w = [NSApp keyWindow];
    if (!w) { for (NSWindow *win in [NSApp windows]) { if (win.isVisible) { w = win; break; } } }
    [w makeKeyAndOrderFront:nil];
    [NSApp activateIgnoringOtherApps:YES];
  });
}
