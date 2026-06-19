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

// statusItemAnchor returns the position to pass to Wails' WindowSetPosition so
// the popover hangs flush under the status item. Wails' SetPosition measures y
// downward from the top of [screen visibleFrame] — which already excludes the
// menu bar — and offsets x by visibleFrame.origin. So y≈0 is flush under the
// menu bar; adding the menu-bar height here (as we used to) double-counts it and
// drops the panel well below the bar.
void statusItemAnchor(int winWidth, int *outX, int *outY) {
  NSRect f = gItem.button.window.frame;                 // status item, bottom-left origin
  NSRect vis = [[NSScreen mainScreen] visibleFrame];
  int cx = (int)(f.origin.x + f.size.width / 2.0);      // status item centre
  *outX = cx - (int)vis.origin.x - winWidth / 2;        // centred under the item
  *outY = 2;                                            // a hair below the menu bar
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
