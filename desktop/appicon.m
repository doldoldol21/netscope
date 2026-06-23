#import <Cocoa/Cocoa.h>
#import <string.h>

// appIconPNG returns malloc'd PNG bytes for the icon of the executable at path
// (a .app bundle's executable resolves to the bundle icon). When path is empty
// it falls back to looking up an application by display name. *outLen receives
// the byte count; returns NULL on failure. The caller owns and frees the buffer.
//
// Runs the AppKit work on the main thread: NSImage rendering isn't thread-safe,
// and the HTTP handler that calls this is on a goroutine. Results are cached in
// Go, so the per-icon main-thread hop happens at most once per app.
const void *appIconPNG(const char *path, const char *name, int px, int *outLen) {
  __block void *result = NULL;
  __block int len = 0;
  dispatch_sync(dispatch_get_main_queue(), ^{
    @autoreleasepool {
      NSWorkspace *ws = [NSWorkspace sharedWorkspace];
      // Resolve to the .app *bundle* path: iconForFile on the Unix executable
      // inside the bundle (.app/Contents/MacOS/Foo) yields a generic "exec"
      // glyph, not the app's real icon. Daemons with no .app fall through to a
      // name lookup, and otherwise to NULL → the UI shows its colored dot.
      NSString *bundle = nil;
      if (path && path[0]) {
        NSString *p = [NSString stringWithUTF8String:path];
        NSRange r = [p rangeOfString:@".app/"];
        if (r.location != NSNotFound) {
          bundle = [p substringToIndex:r.location + 4]; // include ".app"
        } else if ([p hasSuffix:@".app"]) {
          bundle = p;
        }
      }
      if (!bundle && name && name[0]) {
        // Name-based fallback for history rows that carry no exec path. The
        // non-deprecated replacements need a bundle id, which we don't have here.
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
        bundle = [ws fullPathForApplication:[NSString stringWithUTF8String:name]];
#pragma clang diagnostic pop
      }
      if (!bundle) return;
      NSImage *icon = [ws iconForFile:bundle];
      if (!icon) return;
      NSInteger side = px > 0 ? px : 32;
      NSRect rect = NSMakeRect(0, 0, side, side);
      CGImageRef cg = [icon CGImageForProposedRect:&rect context:nil hints:nil];
      if (!cg) return;
      NSBitmapImageRep *rep = [[NSBitmapImageRep alloc] initWithCGImage:cg];
      NSData *png = [rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
      if (!png || png.length == 0) return;
      len = (int)png.length;
      result = malloc(len);
      memcpy(result, png.bytes, len);
    }
  });
  *outLen = len;
  return result;
}
