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

// gFrameCache maps a frame's source pointer -> decoded NSImage. The animator
// re-sends the same ~90 pre-rendered frame buffers forever (stable pointers), so
// decoding the PNG once and reusing the NSImage eliminates a per-frame PNG
// decode + alloc on the main thread (up to 12×/sec).
static NSMutableDictionary *gFrameCache = nil;
// gScreensAsleep is set while the display is asleep so the animator can stop
// swapping frames — the icon isn't visible and redraws only waste battery.
static BOOL gScreensAsleep = NO;

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
    gItem.button.imagePosition = NSImageLeft; // icon, then the live rate text
    gTarget = [[NSStatusTarget alloc] init];
    gItem.button.target = gTarget;
    gItem.button.action = @selector(clicked:);

    // Pause the animation while the display sleeps (lid closed / screen off):
    // the menu bar isn't drawn, so frame swaps are pure waste on battery.
    NSNotificationCenter *wc = [[NSWorkspace sharedWorkspace] notificationCenter];
    [wc addObserverForName:NSWorkspaceScreensDidSleepNotification object:nil
                     queue:[NSOperationQueue mainQueue]
                usingBlock:^(NSNotification *n) { gScreensAsleep = YES; }];
    [wc addObserverForName:NSWorkspaceScreensDidWakeNotification object:nil
                     queue:[NSOperationQueue mainQueue]
                usingBlock:^(NSNotification *n) { gScreensAsleep = NO; }];
  });
}

// menuBarAnimationActive reports whether the animator should keep drawing frames
// (false while the display is asleep). Read from the Go animator loop.
int menuBarAnimationActive(void) { return gScreensAsleep ? 0 : 1; }

// setStatusImage swaps the menu-bar button's template image (one animation
// frame, or the static idle glyph). Kept as a template so macOS tints it for the
// light/dark menu bar automatically. The decoded NSImage is cached by source
// pointer, and an unchanged image is skipped to avoid a needless re-rasterize.
void setStatusImage(const void *png, int len) {
  NSValue *key = [NSValue valueWithPointer:png];
  dispatch_async(dispatch_get_main_queue(), ^{
    if (!gItem) return;
    if (!gFrameCache) gFrameCache = [[NSMutableDictionary alloc] init];
    NSImage *img = gFrameCache[key];
    if (!img) {
      NSData *d = [NSData dataWithBytes:png length:len];
      img = [[NSImage alloc] initWithData:d];
      [img setTemplate:YES];
      [img setSize:NSMakeSize(18, 18)];
      if (img) gFrameCache[key] = img;
    }
    if (img && gItem.button.image != img) gItem.button.image = img;
  });
}

// setStatusText sets the live-rate text shown next to the menu-bar icon. The
// input is a colored-segment protocol: segments joined by US (0x1f), each
// "<tag>:<text>" where tag is d=download, u=upload, n=neutral. An empty string
// clears it (icon only). Monospaced digits keep the width from jittering.
void setStatusText(const char *utf8) {
  NSString *s = utf8 ? [NSString stringWithUTF8String:utf8] : @"";
  dispatch_async(dispatch_get_main_queue(), ^{
    if (!gItem) return;
    if (s.length == 0) {
      gItem.button.attributedTitle = [[NSAttributedString alloc] initWithString:@""];
      return;
    }
    CGFloat size = [NSFont menuBarFontOfSize:0].pointSize; // native menu-bar size
    NSFont *font = [NSFont monospacedDigitSystemFontOfSize:size weight:NSFontWeightRegular];
    NSColor *dl = [NSColor systemGreenColor];
    NSColor *ul = [NSColor systemOrangeColor];
    NSColor *neutral = [NSColor controlTextColor];

    NSMutableAttributedString *out = [[NSMutableAttributedString alloc] init];
    // Leading space separates the text from the icon.
    [out appendAttributedString:[[NSAttributedString alloc] initWithString:@"  "
                                  attributes:@{NSFontAttributeName : font}]];
    for (NSString *seg in [s componentsSeparatedByString:@"\x1f"]) {
      if (seg.length == 0) continue;
      NSColor *color = neutral;
      NSString *text = seg;
      if (seg.length >= 2 && [seg characterAtIndex:1] == ':') {
        unichar tag = [seg characterAtIndex:0];
        if (tag == 'd') color = dl;
        else if (tag == 'u') color = ul;
        text = [seg substringFromIndex:2];
      }
      [out appendAttributedString:[[NSAttributedString alloc] initWithString:text
                                    attributes:@{NSFontAttributeName : font,
                                                 NSForegroundColorAttributeName : color}]];
    }
    gItem.button.attributedTitle = out;
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
